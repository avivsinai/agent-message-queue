package machineagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/mailprompt"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// askPost holds the original reply destination even if the binding changes.
type askPost struct {
	Channel string `json:"channel"`
	// Event is empty while publication is in flight or its outcome is unknown.
	Event   string `json:"event,omitempty"`
	Text    string `json:"text"`
	Root    string `json:"root"`
	From    string `json:"from"`
	Thread  string `json:"thread"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Created string `json:"created"`
}

// A record can repeat the entire subject in its post text. Each input byte
// can expand to a six-byte JSON escape. The remaining fields include the
// bounded body excerpt, binding label and local path.
const maxAskPostSize = 2*6*format.MaxMessageSize + 64*1024

type askState struct {
	quit    <-chan struct{}
	release func()
	edits   map[string]mirrorBackoff
	roots   map[string]askQuiet
	pruneAt time.Time
}

type askQuiet struct {
	dirs  map[string]fsq.FileVersion
	until time.Time
}

func (s *Agent) askLead() bool { return s.workerLead("ask", &s.asks.release) }

func (s *Agent) askDir(name string) string {
	return filepath.Join(s.stateDir, "asks", name)
}

// askTick reduces each live mailbox root once for all its connected handles.
func (s *Agent) askTick(now time.Time) (p pace) {
	targets := map[string][]dialogTarget{}
	for _, t := range s.dialogTargets() {
		if t.binding.Mailbox() {
			targets[t.binding.Root] = append(targets[t.binding.Root], t)
		}
	}
	if s.asks.roots == nil {
		s.asks.roots = map[string]askQuiet{}
	}
	for root := range s.asks.roots {
		if _, ok := targets[root]; !ok {
			delete(s.asks.roots, root)
		}
	}
	p.watch = s.sessionWatch()
	for root, group := range targets {
		dirs := []string{fsq.AgentInboxNew(root, thread.HumanHandle), fsq.AgentInboxCur(root, thread.HumanHandle), fsq.AgentOutboxSent(root, thread.HumanHandle)}
		for _, t := range group {
			dirs = append(dirs, fsq.AgentInboxNew(root, t.binding.Handle), s.askDir(t.name))
		}
		p.watch = append(p.watch, dirs...)
		before, stable := askVersions(dirs, now)
		q := s.asks.roots[root]
		if stable && now.Before(q.until) && maps.Equal(before, q.dirs) {
			p.due(q.until)
			continue
		}
		delete(s.asks.roots, root)
		settled := s.scanAsks(root, group, now)
		if settled && stable {
			if after, ok := askVersions(dirs, time.Now()); ok && maps.Equal(before, after) {
				until := now.Add(sessionViewReconcile)
				s.asks.roots[root] = askQuiet{dirs: before, until: until}
				p.due(until)
				continue
			}
		}
		p.due(now.Add(machineScanInterval))
	}
	if !now.Before(s.asks.pruneAt) {
		s.asks.pruneAt = s.pruneAsks(now)
	}
	p.due(s.asks.pruneAt)
	return p
}

func askVersions(dirs []string, now time.Time) (map[string]fsq.FileVersion, bool) {
	versions := make(map[string]fsq.FileVersion, len(dirs))
	for _, dir := range dirs {
		v, stable, err := fsq.LstatVersion(dir, now)
		if !stable || err != nil {
			return nil, false
		}
		versions[dir] = v
	}
	return versions, true
}

func (s *Agent) scanAsks(root string, targets []dialogTarget, now time.Time) bool {
	cutoff := now.Add(-thread.DefaultObligationMaxAge)
	roots := []thread.ScanRoot{{Root: root}}
	scan, err := thread.CollectAll(roots, cutoff, func(string, error) error { return nil })
	if err != nil {
		return false
	}
	obligations := thread.ReduceObligations(roots, scan.Entries, cutoff, nil).Obligations
	settled := true
	for _, t := range targets {
		byID := map[string]*thread.Obligation{}
		for _, ob := range obligations {
			if ob.Owner != (thread.Participant{Handle: thread.HumanHandle}) || ob.Requester != (thread.Participant{Handle: t.binding.Handle}) {
				continue
			}
			select {
			case <-s.asks.quit:
				return false
			default:
			}
			byID[ob.Opener.ID] = ob
			if ob.Open() && !s.postAsk(t, ob.Opener, now) {
				settled = false
			}
		}
		if !s.closeAsks(t, byID, now) {
			settled = false
		}
	}
	return settled
}

// A claim, including one with an unknown event, prevents a second post.
// Only a definite refusal allows the next scan to try again.
func (s *Agent) postAsk(t dialogTarget, opener *thread.Entry, now time.Time) bool {
	id := opener.ID
	if fsq.ValidateMessageFilename(id+".md") != nil {
		return true
	}
	dir := s.askDir(t.name)
	path := filepath.Join(dir, id+".json")
	for _, file := range []string{path, filepath.Join(dir, "done", id+".json")} {
		if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
			return err == nil
		}
	}
	m, err := format.ReadMessageFile(opener.Occurrences[0].Path)
	if err != nil || m.Header.ID != id {
		return false
	}
	rec := askPost{Channel: t.channel, Text: askText(t.label, m.Header, m.Body, now), Root: t.binding.Root,
		From: t.binding.Handle, Thread: opener.Thread, Kind: opener.Kind, Subject: opener.Subject, Created: opener.Created}
	raw, err := json.Marshal(rec)
	if err != nil {
		return false
	}
	if won, err := createExclusive(path, raw); !won || err != nil {
		return err == nil
	}
	result := s.posts.Send(context.Background(), buzzop.Message{Key: buzzop.Key("ask/" + t.name + "/" + id),
		Channel: t.channel, Content: rec.Text, Mention: s.id.Owner})
	if result.Outcome == buzzop.NotSent {
		_ = os.Remove(path)
		return false
	}
	if result.EventID != "" && (result.Outcome == buzzop.Accepted || result.Outcome == buzzop.Unknown) {
		rec.Event = result.EventID
		raw, err = json.Marshal(rec)
		if err == nil {
			_ = s.writeState(path, raw)
		}
	}
	return true
}

func readAskPosts(dir string) (map[string]askPost, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	posts := make(map[string]askPost)
	var readErr error
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(dir, e.Name()), maxAskPostSize)
		if errors.Is(err, os.ErrNotExist) {
			continue // the worker may have moved it to done
		}
		var rec askPost
		if err == nil {
			err = json.Unmarshal(raw, &rec)
		}
		if err != nil {
			readErr = err
			continue
		}
		posts[id] = rec
	}
	return posts, readErr
}

func (s *Agent) closeAsks(t dialogTarget, byID map[string]*thread.Obligation, now time.Time) bool {
	dir := s.askDir(t.name)
	doneDir := filepath.Join(dir, "done")
	posts, err := readAskPosts(dir)
	settled := err == nil
	for id, rec := range posts {
		select {
		case <-s.asks.quit:
			return false
		default:
		}
		ob := byID[id]
		if rec.Root != t.binding.Root || rec.From != t.binding.Handle || ob != nil && ob.Open() {
			continue
		}
		path := filepath.Join(dir, id+".json")
		if now.Before(s.asks.edits[path].until) {
			settled = false
			continue
		}
		if rec.Event != "" {
			result := s.posts.Edit(context.Background(), buzzop.Key("ask/"+t.name+"/"+id+"/closed"), rec.Channel, rec.Event,
				rec.Text+"\n\n"+askOutcome(ob))
			if result.Outcome != buzzop.Accepted {
				backOffEdit(&s.asks.edits, path, now)
				settled = false
				continue
			}
		}
		delete(s.asks.edits, path)
		if os.MkdirAll(doneDir, 0o700) != nil || os.Rename(path, filepath.Join(doneDir, id+".json")) != nil {
			settled = false
		}
	}
	return settled
}

// Records outlive a disconnected binding, so retention covers every name.
// A newly posted opener is at most DefaultObligationMaxAge old and thus
// cannot need pruning in the next day.
func (s *Agent) pruneAsks(now time.Time) time.Time {
	next := now.Add(24 * time.Hour)
	root := filepath.Join(s.stateDir, "asks")
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				next = earliest(next, now.Add(sessionViewReconcile))
			}
			return nil
		}
		if path == root {
			return nil
		}
		parent := filepath.Dir(path)
		if e.IsDir() {
			if parent == root || e.Name() == "done" && filepath.Dir(parent) == root {
				return nil
			}
			return filepath.SkipDir
		}
		if parent == root || !e.Type().IsRegular() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		select {
		case <-s.asks.quit:
			return filepath.SkipAll
		default:
		}
		var record struct{ Created time.Time }
		raw, err := fsq.ReadRegularNoFollowBounded(path, maxAskPostSize)
		if err != nil || json.Unmarshal(raw, &record) != nil || record.Created.IsZero() {
			return nil
		}
		until := record.Created.Add(thread.DefaultObligationMaxAge + 24*time.Hour)
		if !now.After(until) {
			next = earliest(next, until)
		} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			next = earliest(next, now.Add(sessionViewReconcile))
		} else {
			delete(s.asks.edits, path)
		}
		return nil
	})
	if err != nil {
		return now.Add(sessionViewReconcile)
	}
	return next
}

func askOutcome(ob *thread.Obligation) string {
	if ob == nil || ob.Expired || ob.ClosedBy == nil {
		return "→ Expired"
	}
	if ob.ClosedBy.From == thread.HumanHandle {
		if slices.Contains(ob.ClosedBy.Labels, "buzz") {
			return "→ Answered (Buzz)"
		}
		return "→ Answered"
	}
	return "→ Closed by " + ob.Requester.Handle
}

func askText(label string, h format.Header, body string, _ time.Time) string {
	if label == "" {
		label = "A session"
	}
	action, deadline := "asks:", "Default at %s."
	switch h.Kind {
	case format.KindQuestion:
		if slices.Contains(h.Labels, "ruling") {
			action, deadline = "decided:", "Stands at %s unless you reply."
		}
	case format.KindTodo:
		action, deadline = "has a task for you:", "Reply by %s."
	case format.KindReviewRequest:
		action, deadline = "asks for a review:", "Reply by %s."
	}
	var b strings.Builder
	b.WriteString(label + " " + action)
	if h.Subject != "" {
		b.WriteString("\n" + h.Subject)
	}
	body = strings.TrimSpace(body)
	if len(body) > dialogTextMax {
		cut := dialogTextMax
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut] + "\n… full text: amq read --id " + h.ID
	}
	if body != "" {
		b.WriteString("\n\n" + body)
	}
	b.WriteString("\n\n")
	if at, err := time.Parse(time.RFC3339, h.ReplyBy); err == nil {
		fmt.Fprintf(&b, deadline+"\n", at.Local().Format("Mon 2 Jan 15:04 MST"))
	} else if h.Kind == format.KindQuestion {
		b.WriteString("No default: this waits for you.\n")
	}
	b.WriteString("Reply in this thread to answer.")
	return b.String()
}

type askReply struct {
	post  askPost
	id    string
	done  bool
	text  string
	event nostr.Event
}

func (s *Agent) askReplyOf(m ownerMsg) (decision, bool) {
	refs, tagged := replyRefs(m.event)
	if !tagged || len(refs) == 0 {
		return decision{}, false
	}
	r, ok := s.channelRoute(m.channel)
	if !ok {
		return decision{}, false
	}
	dir := s.askDir(r.Name)
	var a askReply
	for _, folder := range []string{dir, filepath.Join(dir, "done")} {
		posts, err := readAskPosts(folder)
		if err != nil {
			return decision{}, false
		}
		for id, post := range posts {
			if post.Channel != m.channel || post.Event == "" || !slices.Contains(refs, post.Event) {
				continue
			}
			// A record can move to done between the two directory reads.
			if a.id != "" && (a.id != id || a.post != post || !notExist(filepath.Join(dir, id+".json"))) {
				return decision{}, false
			}
			a = askReply{post: post, id: id, done: folder != dir}
		}
	}
	created, err := time.Parse(time.RFC3339, a.post.Created)
	if a.id == "" || err != nil || !signedWithin(m.event, created.Add(-ownerSkew), time.Now()) {
		return decision{}, false
	}
	text := dropMentions(m.event.Content)
	if text == "" {
		return decision{}, false
	}
	a.text, a.event = unfetchedImages(m.event, text), m.event
	return decision{kind: decidedAsk, ask: a}, true
}

func (a askReply) deliver() (string, error) {
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err == nil {
		rec := a.post
		kind, labels := thread.ClosingReply(rec.Kind)
		subject := rec.Subject
		if !strings.HasPrefix(subject, "Re: ") {
			subject = "Re: " + subject
		}
		err = publishPromptAt(rec.Root, id, now, mailprompt.Prompt{To: rec.From, From: thread.HumanHandle,
			Thread: rec.Thread, Subject: subject, Kind: kind, Labels: append(labels, "buzz", "nostr:"+a.event.ID.Hex()),
			Refs: []string{a.id}, Context: eventContext(a.event), Body: a.text})
	}
	if err != nil {
		return fmt.Sprintf("Not delivered to %s: %v.", a.post.From, err), err
	}
	if a.done {
		return "Delivered. This request was already closed.", nil
	}
	return "Answered.", nil
}
