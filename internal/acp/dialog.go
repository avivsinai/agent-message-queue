package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// The Buzz agent mirrors every harness dialog of the sessions it serves.
// A harness seam opens a request under the binding's dialogs directory
// (package dialog); the worker that leads this agent posts it once into the
// session's channel (machine agent) or DM (pinned agent), turns the owner's
// ✅ or ❌ reaction or typed reply into the answer file, and edits the post
// with the outcome once the dialog is resolved, by Buzz or in the terminal.
// The first answer wins: answers and resolutions are create-exclusive.
//
// A post record <state>/dialogs/<binding>/<id>.json is created before the
// post, so a dialog is posted at most once across workers and restarts; it
// moves to done/ once the post shows the outcome.

// dialogTextMax bounds the dialog body in a post; the options are still
// offered under a cut body.
const dialogTextMax = 3500

// reactionPollInterval is the gap between reads of the owner's reactions on
// one posted dialog.
var reactionPollInterval = 3 * time.Second

// buzzOwner is the managed agent's owner pubkey (64 lowercase hex), or ""
// when it cannot be established. Only its reactions and messages answer a
// dialog.
var buzzOwner string

// agentOwner is the owner that Buzz Desktop gave this agent: the NIP-OA
// BUZZ_AUTH_TAG ["auth", owner, conditions, sig], verified against the
// agent's own key as buzz-acp verifies it (resolve_agent_owner), else
// BUZZ_ACP_AGENT_OWNER, which Desktop sets only for an agent with no auth
// tag. A tag that does not verify yields no owner.
func agentOwner(authTag, secret, fallback string) string {
	if authTag = strings.TrimSpace(authTag); authTag != "" {
		var tag []string
		if json.Unmarshal([]byte(authTag), &tag) != nil {
			return ""
		}
		t, err := bodykey.ParseAuthTag(tag)
		if err != nil {
			return ""
		}
		agent, err := agentPubKey(strings.TrimSpace(secret))
		if err != nil || t.Verify(agent) != nil {
			return ""
		}
		return t.OwnerPubKey
	}
	if fallback = strings.ToLower(strings.TrimSpace(fallback)); len(fallback) == 64 {
		if _, err := nostr.PubKeyFromHex(fallback); err == nil {
			return fallback
		}
	}
	return ""
}

// agentPubKey is the hex public key of a hex or nsec secret.
func agentPubKey(secret string) (string, error) {
	sk, err := agentSecret(secret)
	if err != nil {
		return "", err
	}
	return sk.Public().Hex(), nil
}

// agentSecret parses a hex or nsec secret.
func agentSecret(secret string) (nostr.SecretKey, error) {
	if strings.HasPrefix(secret, "nsec1") {
		prefix, val, err := nip19.Decode(secret)
		sk, ok := val.(nostr.SecretKey)
		if err != nil || prefix != "nsec" || !ok {
			return nostr.SecretKey{}, errors.New("invalid nsec")
		}
		return sk, nil
	}
	return nostr.SecretKeyFromHex(secret)
}

// reaction is one emoji on a message and who reacted with it.
type reaction struct {
	Emoji   string   `json:"emoji"`
	PubKeys []string `json:"pubkeys"`
}

// readReactions reads the reactions on one event. A variable so tests can
// supply reactions without a relay.
var readReactions = reactionsWithBuzzCLI

// reactionsWithBuzzCLI runs `buzz reactions get --event <id>`, which prints
// {"reactions":[{"emoji","count","pubkeys"}]}.
func reactionsWithBuzzCLI(eventID string, budget time.Duration) ([]reaction, error) {
	out, err := runBuzzCLI(budget, nil, "reactions", "get", "--event", eventID)
	if err != nil {
		return nil, err
	}
	var got struct {
		Reactions []reaction `json:"reactions"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("buzz reactions get: %v", err)
	}
	return got.Reactions, nil
}

// reacted reports whether owner reacted with emoji.
func reacted(rs []reaction, owner, emoji string) bool {
	for _, r := range rs {
		if strings.TrimSpace(r.Emoji) == emoji && slices.Contains(r.PubKeys, owner) {
			return true
		}
	}
	return false
}

// editMessage replaces the content of a message this agent posted. A
// variable so tests can record edits without a relay.
var editMessage = editWithBuzzCLI

// editWithBuzzCLI runs `buzz messages edit --event <id> --content <text>`.
func editWithBuzzCLI(eventID, content string, budget time.Duration) error {
	_, err := runBuzzCLI(budget, nil, "messages", "edit", "--event", eventID, "--content", content)
	return err
}

// dialogPost is the record of one posted dialog.
type dialogPost struct {
	Channel string `json:"channel"`
	// Event is the posted message; "" while the post is in flight or when
	// its outcome is unknown. Such a dialog is never posted again.
	Event string `json:"event,omitempty"`
	Text  string `json:"text"`
}

// dialogTarget is one binding whose dialogs this worker posts, and where.
type dialogTarget struct {
	name, channel, label string
}

// dialogState is the poster's state, owned by the one worker goroutine that
// runs dialogTick.
type dialogState struct {
	polled map[string]time.Time // dialog id -> last reaction read
	// release drops a pinned agent's dialog lead; nil when not held.
	release func()
}

// dialogKeep is how long a resolved dialog's files are kept.
const dialogKeep = time.Hour

// dialogTick posts the open dialogs of every live binding this worker
// serves, reads the owner's reactions on them, and edits the posts of
// dialogs that were resolved. A dialog whose seam process is gone expires.
// Resolved dialogs are dropped after dialogKeep, and the dialogs of a
// binding that is gone expire and are removed.
func (s *Server) dialogTick(now time.Time) {
	for _, t := range s.dialogTargets() {
		if s.stopped() {
			return
		}
		dir, err := dialog.DirFor(t.name)
		if err != nil {
			continue
		}
		reqs, err := dialog.Requests(dir)
		if err != nil {
			continue
		}
		open := map[string]bool{}
		for _, req := range reqs {
			if req.PID > 0 && !pidAlive(req.PID) {
				_, _ = dialog.Resolve(dir, dialog.Resolved{ID: req.ID, Outcome: dialog.OutcomeExpired, By: dialog.ByTimeout, At: now.UTC()})
				continue
			}
			open[req.ID] = true
			s.dialogStep(t, dir, req, now)
		}
		s.dialogClose(t.name, dir, open)
		s.pruneDialogs(t.name, dir, now)
	}
	if !s.stopped() {
		s.dropGoneDialogs(now)
	}
}

// dialogTargets are the bindings this worker posts dialogs for: a machine
// agent's live channels and its approvals channel, or a pinned agent's live
// binding and its latest DM.
func (s *Server) dialogTargets() []dialogTarget {
	if buzzSelf == "" {
		return nil
	}
	if s.cfg.Machine {
		var out []dialogTarget
		for _, r := range s.activeRecords() {
			if r.State != channelActive || r.ChannelID == "" || r.Machine != buzzSelf || r.Relay != normalizeRelay(buzzRelayURL) {
				continue
			}
			if r.Name == dialog.MachineBinding {
				out = append(out, dialogTarget{name: r.Name, channel: r.ChannelID})
				continue
			}
			if b, err := binding.ReadNamed(r.Name); err == nil && b.SessionIdentity() == r.Identity && live(b) {
				out = append(out, dialogTarget{name: r.Name, channel: r.ChannelID, label: sessionAgentName(b)})
			}
		}
		return out
	}
	if s.cfg.Helper || s.cfg.Pin == "" || s.cfg.PinErr != nil {
		return nil
	}
	b, err := binding.ReadNamed(s.cfg.Pin)
	if err != nil || !live(b) {
		return nil
	}
	channel := ""
	if raw, err := readSmallRegular(filepath.Join(s.agentMarkerDir(s.cfg.Pin), buzzSelf+".dm")); err == nil {
		channel = strings.TrimSpace(string(raw))
	}
	if channel == "" {
		s.mu.Lock()
		channel = s.lastChannel
		s.mu.Unlock()
	}
	if channel == "" {
		return nil
	}
	return []dialogTarget{{name: s.cfg.Pin, channel: channel, label: sessionAgentName(b)}}
}

// live reports whether b's session may still run: it is not proven ended.
// A binding written before holders were recorded has unknown liveness.
func live(b binding.Binding) bool {
	state, _ := b.Liveness()
	return state != binding.Ended
}

// dropGoneDialogs expires the open dialogs of each binding that is gone or
// ended, edits their posts, and removes the binding's dialogs. A machine
// agent does this for every binding, a pinned agent for its own; the
// machine's own dialogs belong to no binding and are never removed.
func (s *Server) dropGoneDialogs(now time.Time) {
	base, err := dialog.DirFor(dialog.MachineBinding)
	if err != nil {
		return
	}
	root := filepath.Dir(base)
	var names []string
	if s.cfg.Machine {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() && e.Name() != dialog.MachineBinding {
				names = append(names, e.Name())
			}
		}
	} else if s.cfg.Pin != "" {
		names = []string{s.cfg.Pin}
	}
	for _, name := range names {
		dir := filepath.Join(root, name)
		if _, err := os.Lstat(dir); err != nil {
			continue
		}
		b, err := binding.ReadNamed(name)
		switch {
		case errors.Is(err, binding.ErrNone):
		case err == nil && !live(b):
		default:
			continue
		}
		reqs, _ := dialog.Requests(dir)
		for _, req := range reqs {
			_, _ = dialog.Resolve(dir, dialog.Resolved{ID: req.ID, Outcome: dialog.OutcomeExpired, By: dialog.ByTimeout, At: now.UTC()})
		}
		s.dialogClose(name, dir, nil)
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(s.dialogPostDir(name))
	}
}

// pruneDialogs removes the files of each dialog resolved more than
// dialogKeep ago, the request first, so it never reads as open again.
func (s *Server) pruneDialogs(name, dir string, now time.Time) {
	entries, _ := os.ReadDir(filepath.Join(dir, "resolved"))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		res, found, err := dialog.ReadResolved(dir, id)
		if err != nil || !found || now.Sub(res.At) <= dialogKeep {
			continue
		}
		if os.Remove(filepath.Join(dir, "requests", e.Name())) != nil && !notExist(filepath.Join(dir, "requests", e.Name())) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, "answers", e.Name()))
		_ = os.Remove(filepath.Join(s.dialogPostDir(name), e.Name()))
		_ = os.Remove(filepath.Join(s.dialogPostDir(name), "done", e.Name()))
		_ = os.Remove(filepath.Join(dir, "resolved", e.Name()))
	}
}

func notExist(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func (s *Server) dialogPostDir(name string) string {
	return filepath.Join(s.cfg.StateDir, "dialogs", name)
}

// dialogStep posts req once, then reads the owner's reactions on the post
// every reactionPollInterval until it is answered: ❌ picks the deny option
// and beats ✅, which picks option 1.
func (s *Server) dialogStep(t dialogTarget, dir string, req dialog.Request, now time.Time) {
	path := filepath.Join(s.dialogPostDir(t.name), req.ID+".json")
	p, found := readDialogPost(path)
	if !found {
		s.postDialog(path, t, req)
		return
	}
	if p.Event == "" {
		return
	}
	if _, answered, err := dialog.ReadAnswer(dir, req.ID); err != nil || answered {
		return
	}
	if s.dialogs.polled == nil {
		s.dialogs.polled = map[string]time.Time{}
	}
	if last, ok := s.dialogs.polled[req.ID]; ok && now.Sub(last) < reactionPollInterval {
		return
	}
	s.dialogs.polled[req.ID] = now
	rs, err := readReactions(p.Event, s.cfg.PostTimeout)
	if err != nil {
		return
	}
	for _, emoji := range []string{"❌", "✅"} {
		if !reacted(rs, buzzOwner, emoji) {
			continue
		}
		if a, ok := dialog.ParseAnswer(req, emoji); ok {
			_, _ = dialog.WriteAnswer(dir, a)
			return
		}
	}
}

// postDialog claims req's post record, posts it and records the message. A
// post proved not sent drops the claim so the next tick posts again; any
// other failure leaves the outcome unknown, and the dialog is not posted
// again.
func (s *Server) postDialog(path string, t dialogTarget, req dialog.Request) {
	label := req.Label
	if label == "" {
		label = t.label
	}
	if label == "" {
		label = "A session"
	}
	p := dialogPost{Channel: t.channel, Text: dialogText(label, req)}
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	if won, err := createExclusive(path, raw); err != nil || !won {
		return
	}
	id, err := postAnswer(t.channel, p.Text, s.cfg.PostTimeout)
	var notSent *notSentError
	if errors.As(err, &notSent) || errors.Is(err, errNoBuzzIdentity) {
		_ = os.Remove(path)
		return
	}
	if err != nil || id == "" {
		return
	}
	p.Event = id
	_ = writeDialogPost(path, p)
}

// dialogClose edits the post of each dialog that is no longer open and has
// a resolution with its outcome, and moves its record to done/. A failed
// edit is tried again on the next tick.
func (s *Server) dialogClose(name, dir string, open map[string]bool) {
	postDir := s.dialogPostDir(name)
	entries, _ := os.ReadDir(postDir)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || open[id] {
			continue
		}
		res, resolved, err := dialog.ReadResolved(dir, id)
		if err != nil || !resolved {
			continue
		}
		path := filepath.Join(postDir, e.Name())
		p, found := readDialogPost(path)
		if !found {
			continue
		}
		if p.Event != "" && editMessage(p.Event, p.Text+"\n\n"+outcomeLine(res), s.cfg.PostTimeout) != nil {
			continue
		}
		delete(s.dialogs.polled, id)
		done := filepath.Join(postDir, "done")
		if os.MkdirAll(done, 0o700) == nil {
			_ = os.Rename(path, filepath.Join(done, e.Name()))
		}
	}
}

func readDialogPost(path string) (dialogPost, bool) {
	raw, err := readSmallRegular(path)
	if err != nil {
		return dialogPost{}, false
	}
	var p dialogPost
	if json.Unmarshal(raw, &p) != nil {
		return dialogPost{}, false
	}
	return p, true
}

func writeDialogPost(path string, p dialogPost) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), raw, 0o600)
	return err
}

// dialogText is the Buzz message for one dialog: who asks, the dialog body,
// the numbered options and how to answer.
func dialogText(label string, req dialog.Request) string {
	var b strings.Builder
	if req.Tool != "" {
		fmt.Fprintf(&b, "%s wants to run %s:\n", label, req.Tool)
	} else {
		fmt.Fprintf(&b, "%s asks:\n", label)
	}
	body := strings.TrimSpace(req.Text)
	if len(body) > dialogTextMax {
		cut := dialogTextMax
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut] + "\n… full text in the terminal"
	}
	if body != "" {
		b.WriteString(body + "\n")
	}
	deny := false
	if len(req.Options) > 0 {
		b.WriteString("\n")
	}
	for _, o := range req.Options {
		fmt.Fprintf(&b, "%d. %s\n", o.N, o.Label)
		deny = deny || o.Deny
	}
	var how []string
	if len(req.Options) > 0 {
		how = append(how, "React ✅ = 1")
		if deny {
			how = append(how, "❌ = Deny")
		}
		how = append(how, "or reply a number / yes / no")
	}
	if req.FreeText {
		how = append(how, "or reply with your answer")
	}
	if len(how) == 0 {
		how = append(how, "Answer in the terminal")
	}
	b.WriteString(strings.Join(how, ", ") + ".")
	return b.String()
}

// outcomeLine is the line a resolved dialog's post gains.
func outcomeLine(r dialog.Resolved) string {
	switch {
	case r.By == dialog.ByTerminal || r.Outcome == dialog.OutcomeTerminal:
		return "→ Answered in the terminal"
	case r.By == dialog.ByTimeout || r.Outcome == dialog.OutcomeExpired:
		return "→ Expired"
	}
	label := r.Label
	if label == "" {
		switch r.Outcome {
		case dialog.OutcomeAllowed:
			label = "Allowed"
		case dialog.OutcomeDenied:
			label = "Denied"
		default:
			label = "Answered"
		}
	}
	return "→ " + label + " (Buzz)"
}

// dialogLead reports whether this pinned agent's worker posts its dialogs:
// the worker that holds <pin>/<self>.dialog.lock. A machine agent's dialogs
// are posted by its machine leader instead.
func (s *Server) dialogLead() bool {
	if s.dialogs.release != nil || !lock.AdvisoryLockAvailable() {
		return true
	}
	dir := s.agentMarkerDir(s.cfg.Pin)
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	release, ok, err := lock.TryExclusive(filepath.Join(dir, buzzSelf+".dialog.lock"))
	if err != nil || !ok {
		return false
	}
	s.dialogs.release = release
	return true
}

// dialogReply returns the answer that an owner's Buzz message in channel
// gives to an open dialog posted there, and the dialog's directory. A thread
// reply to a dialog's post answers that dialog; any other message answers
// only when exactly one dialog is open in channel. A message from someone
// other than the owner never answers.
func (s *Server) dialogReply(channel, prompt string) (string, dialog.Answer, bool) {
	if channel == "" {
		return "", dialog.Answer{}, false
	}
	name := s.cfg.Pin
	if s.cfg.Machine {
		r, ok := s.channelRoute(channel)
		if !ok {
			return "", dialog.Answer{}, false
		}
		name = r.Name
	} else if s.cfg.Helper || name == "" || s.cfg.PinErr != nil {
		return "", dialog.Answer{}, false
	}
	if from := eventSender(prompt); from != "" && buzzOwner != "" && from != buzzOwner {
		return "", dialog.Answer{}, false
	}
	dir, err := dialog.DirFor(name)
	if err != nil {
		return "", dialog.Answer{}, false
	}
	reqs, err := dialog.Requests(dir)
	if err != nil {
		return "", dialog.Answer{}, false
	}
	refs := threadRefs(prompt)
	var here []dialog.Request
	for _, req := range reqs {
		p, found := readDialogPost(filepath.Join(s.dialogPostDir(name), req.ID+".json"))
		if !found || p.Channel != channel {
			continue
		}
		if p.Event != "" && slices.Contains(refs, p.Event) {
			here = []dialog.Request{req}
			break
		}
		if len(refs) == 0 {
			here = append(here, req)
		}
	}
	if len(here) != 1 {
		return "", dialog.Answer{}, false
	}
	a, ok := dialog.ParseAnswer(here[0], ownerWords(prompt))
	return dir, a, ok
}

// answerDialog writes a typed answer, tells the owner it was taken, and ends
// the prompt without delivering it to the session.
func (s *Server) answerDialog(sessionID, channel, dir string, a dialog.Answer, emit func(any) error) (any, *rpcError) {
	text := "Answered."
	won, err := dialog.WriteAnswer(dir, a)
	switch {
	case err != nil:
		text = "The answer could not be recorded: " + err.Error()
	case !won:
		text = "Already answered."
	}
	meta := remoteMeta{State: "answered", Posted: s.publish(channel, text)}
	if err := emitText(emit, sessionID, "agent_message_chunk", text); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
	}
	return remotePromptResult{StopReason: StopReasonEndTurn, Meta: remotePromptMeta{Remote: meta}}, nil
}

// buzzEvent returns the one <buzz-event> block of a buzz-acp prompt, or "".
func buzzEvent(prompt string) string {
	start := strings.Index(prompt, "<buzz-event")
	if start < 0 || strings.Count(prompt, "<buzz-event") != 1 {
		return ""
	}
	end := strings.Index(prompt[start:], "</buzz-event>")
	if end < 0 {
		return ""
	}
	return prompt[start : start+end]
}

// ownerWords is the text the owner typed: the Content of the prompt's one
// Buzz event, else what follows the <context> block, with leading mentions
// dropped.
func ownerWords(prompt string) string {
	text := prompt
	if evt := buzzEvent(prompt); evt != "" {
		_, content, ok := strings.Cut(evt, "\nContent: ")
		if !ok {
			return ""
		}
		var lines []string
		for line := range strings.SplitSeq(content, "\n") {
			if strings.HasPrefix(line, "Tags: ") || strings.HasPrefix(line, "Parsed: ") {
				break
			}
			lines = append(lines, line)
		}
		text = strings.Join(lines, "\n")
	} else if _, after, ok := strings.Cut(prompt, "</context>"); ok {
		text = after
	}
	fields := strings.Fields(text)
	for len(fields) > 1 && (strings.HasPrefix(fields[0], "@") || strings.HasPrefix(fields[0], "nostr:")) {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}

// eventSender is the hex pubkey on the From line of the prompt's Buzz
// event, or "".
func eventSender(prompt string) string {
	for line := range strings.SplitSeq(buzzEvent(prompt), "\n") {
		if rest, ok := strings.CutPrefix(line, "From: "); ok {
			if _, hex, ok := strings.Cut(rest, "hex: "); ok {
				return strings.TrimSuffix(strings.TrimSpace(hex), ")")
			}
		}
	}
	return ""
}

// threadRefs are the event ids the prompt's message replies to: the Thread
// root and Parent lines of its <context> block and the e tags of its Buzz
// event.
func threadRefs(prompt string) []string {
	var refs []string
	add := func(id string) {
		if id = strings.ToLower(strings.TrimSpace(id)); nostrEventIDRe.MatchString(id) && !slices.Contains(refs, id) {
			refs = append(refs, id)
		}
	}
	if start, end := strings.Index(prompt, "<context>"), strings.Index(prompt, "</context>"); start >= 0 && end > start {
		for line := range strings.SplitSeq(prompt[start:end], "\n") {
			if rest, ok := strings.CutPrefix(line, "Thread root: "); ok {
				add(rest)
			} else if rest, ok := strings.CutPrefix(line, "Parent: "); ok {
				add(rest)
			}
		}
	}
	for line := range strings.SplitSeq(buzzEvent(prompt), "\n") {
		rest, ok := strings.CutPrefix(line, "Tags: ")
		if !ok {
			continue
		}
		var tags [][]string
		if json.Unmarshal([]byte(rest), &tags) != nil {
			continue
		}
		for _, t := range tags {
			if len(t) >= 2 && t[0] == "e" {
				add(t[1])
			}
		}
	}
	return refs
}
