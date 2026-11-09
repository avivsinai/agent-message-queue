package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzevent"
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
// one posted dialog while it is fresh.
var reactionPollInterval = 3 * time.Second

const (
	// reactionPollFresh is how long a dialog is fresh; each further span of
	// that length doubles its gap between reaction reads.
	reactionPollFresh = 2 * time.Minute
	// reactionPollMax caps the gap between reaction reads.
	reactionPollMax = time.Minute
)

// reactionPollGap is the gap between reaction reads of a dialog open for
// age: reactionPollInterval while it is fresh, then doubling every
// reactionPollFresh up to reactionPollMax. Each read starts a process, and
// a dialog left open for long is seldom answered from Buzz.
func reactionPollGap(age time.Duration) time.Duration {
	gap := reactionPollInterval
	for n := age / reactionPollFresh; n > 0 && gap < reactionPollMax; n-- {
		gap *= 2
	}
	return min(gap, reactionPollMax)
}

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
	// binding is the target's binding as dialogTargets read it.
	binding binding.Binding
}

// dialogState is the poster's state, owned by the one worker goroutine that
// runs dialogTick.
type dialogState struct {
	polled map[string]time.Time // dialog id -> last reaction read
	// edits holds the backoff of each dialog whose post edit failed.
	edits map[string]mirrorBackoff
	// quiet holds each binding whose last full pass left nothing to do;
	// its directories are not read again until one of them changes or the
	// entry is due.
	quiet map[string]dialogQuiet
	// quit closes when the dialog worker stops; nil outside it.
	quit <-chan struct{}
	// release drops a pinned agent's dialog lead; nil when not held.
	release func()
}

// dialogKeep is how long a resolved dialog's files are kept.
const dialogKeep = time.Hour

// A failed post edit is tried again after dialogEditBackoffMin, doubling to
// dialogEditBackoffMax.
const (
	dialogEditBackoffMin = 4 * time.Second
	dialogEditBackoffMax = 5 * time.Minute
)

// dialogTick posts the open dialogs of every live binding this worker
// serves, newest first, reads the owner's reactions on them, and edits the
// posts of dialogs that were resolved. A dialog whose seam process is gone
// expires. Resolved dialogs are dropped after dialogKeep, and the dialogs of
// a binding that is gone expire and are removed.
func (s *Server) dialogTick(now time.Time) {
	type scope struct {
		t     dialogTarget
		dir   string
		open  map[string]bool
		key   [3]fsq.FileVersion // as of before the pass; keyed false when unusable
		keyed bool
	}
	type step struct {
		t   dialogTarget
		dir string
		req dialog.Request
	}
	var scopes []scope
	var steps []step
	served := map[string]bool{}
	for _, t := range s.dialogTargets() {
		if s.dialogStopped() {
			return
		}
		served[t.name] = true
		if s.dialogsQuiet(t.name, now) {
			continue
		}
		dir, err := dialog.DirFor(t.name)
		if err != nil {
			continue
		}
		key, keyed := s.dialogQuietKey(t.name, dir, now)
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
			steps = append(steps, step{t, dir, req})
		}
		scopes = append(scopes, scope{t, dir, open, key, keyed})
	}
	// Each step may wait on the relay, so a dialog left open never delays
	// the post or the reactions of a newer one.
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].req.OpenedAt.After(steps[j].req.OpenedAt) })
	for _, st := range steps {
		if s.dialogStopped() {
			return
		}
		s.dialogStep(st.t, st.dir, st.req, now)
	}
	for _, sc := range scopes {
		if s.dialogStopped() {
			return
		}
		closed := s.dialogClose(sc.t.name, sc.dir, sc.open, now)
		due := s.pruneDialogs(sc.t.name, sc.dir, now)
		if len(sc.open) == 0 && closed && sc.keyed {
			s.markDialogsQuiet(sc.t.name, sc.dir, sc.key, earliest(due, now.Add(sessionViewReconcile)))
		}
	}
	if !s.dialogStopped() {
		s.dropGoneDialogs(served, now)
	}
}

// dialogQuiet is a binding whose dialogs need nothing until key changes or
// until: no dialog is open, every post was edited, and no resolved dialog
// is due to be pruned before until.
type dialogQuiet struct {
	key   [3]fsq.FileVersion
	until time.Time
}

// dialogQuietKey is the version of the directories a seam or a post
// changes: requests, resolved and the post directory. Answers and events
// matter only to an open dialog, and a binding with one is never quiet. ok
// is false when one could not be read or changed too recently to stand for
// its content.
func (s *Server) dialogQuietKey(name, dir string, now time.Time) (key [3]fsq.FileVersion, ok bool) {
	ok = true
	for i, p := range []string{filepath.Join(dir, "requests"), filepath.Join(dir, "resolved"), s.dialogPostDir(name)} {
		v, settled, err := fsq.LstatVersion(p, now)
		key[i] = v
		ok = ok && settled && err == nil
	}
	return key, ok
}

// markDialogsQuiet records that name's dialogs need nothing before until,
// when its directories still have the version read before the pass: a pass
// that changed them, or a seam that wrote meanwhile, leaves it unmarked.
func (s *Server) markDialogsQuiet(name, dir string, before [3]fsq.FileVersion, until time.Time) {
	key, ok := s.dialogQuietKey(name, dir, time.Now())
	if !ok || key != before {
		delete(s.dialogs.quiet, name)
		return
	}
	if s.dialogs.quiet == nil {
		s.dialogs.quiet = map[string]dialogQuiet{}
	}
	s.dialogs.quiet[name] = dialogQuiet{key: key, until: until}
}

// dialogsQuiet reports whether name's dialogs still need nothing: none of
// its directories changed since its last full pass, and nothing is due.
func (s *Server) dialogsQuiet(name string, now time.Time) bool {
	q, ok := s.dialogs.quiet[name]
	if !ok {
		return false
	}
	root, err := dialog.Root()
	if err == nil && now.Before(q.until) {
		if key, ok := s.dialogQuietKey(name, filepath.Join(root, name), now); ok && key == q.key {
			return true
		}
	}
	delete(s.dialogs.quiet, name)
	return false
}

// dialogStopped reports whether the dialog worker is stopping.
func (s *Server) dialogStopped() bool {
	select {
	case <-s.dialogs.quit:
		return true
	default:
		return false
	}
}

// routable reports whether r is an active channel record of this machine
// agent on its relay.
func routable(r channelRecord) bool {
	return r.State == channelActive && r.ChannelID != "" && r.Machine == buzzSelf && r.Relay == normalizeRelay(buzzRelayURL)
}

// serves reports whether the routable record r is the channel of b's live
// session.
func (s *Server) serves(r channelRecord, b binding.Binding) bool {
	return b.SessionIdentity() == r.Identity && s.live(b)
}

// targetCache holds a machine agent's dialog targets for one session
// snapshot and one machine tick's app-server judge, so the dialog and mirror
// workers, which tick at the same pace, judge each binding's liveness once.
type targetCache struct {
	mu      sync.Mutex
	snap    *sessionSnapshot
	judge   *appServerJudge
	at      time.Time
	targets []dialogTarget
}

// machineTargets are a machine agent's live channels. They are judged again
// when the snapshot or the machine tick changes, and at least every
// machineScanInterval. Callers must not modify the result.
func (s *Server) machineTargets() []dialogTarget {
	c := &s.targets
	c.mu.Lock()
	defer c.mu.Unlock()
	snap, judge, now := s.snapshot(), s.appServers.Load(), time.Now()
	if c.snap == snap && c.judge == judge && !now.Before(c.at) && now.Sub(c.at) < machineScanInterval {
		return c.targets
	}
	var out []dialogTarget
	for _, r := range snap.activeRecords() {
		if !routable(r) {
			continue
		}
		if b, ok := snap.binding(r.Name); ok && s.serves(r, b) {
			out = append(out, dialogTarget{name: r.Name, channel: r.ChannelID, label: sessionAgentName(b), binding: b})
		}
	}
	c.snap, c.judge, c.at, c.targets = snap, judge, now, out
	return out
}

// dialogTargets are the bindings this worker posts dialogs for: a machine
// agent's live channels, or a pinned agent's live binding and its latest DM.
func (s *Server) dialogTargets() []dialogTarget {
	if buzzSelf == "" {
		return nil
	}
	if s.cfg.Machine {
		if buzzRelayKey == "" || buzzOwner == "" {
			return nil
		}
		return s.machineTargets()
	}
	if s.cfg.Helper || s.cfg.Pin == "" || s.cfg.PinErr != nil {
		return nil
	}
	b, err := binding.ReadNamed(s.cfg.Pin)
	if err != nil || !s.live(b) {
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
	return []dialogTarget{{name: s.cfg.Pin, channel: channel, label: sessionAgentName(b), binding: b}}
}

// live reports whether b's session may still run: it is not proven ended.
// A binding written before holders were recorded has unknown liveness.
func (s *Server) live(b binding.Binding) bool {
	state, _ := s.liveness(b)
	return state != binding.Ended
}

// dropGoneDialogs expires the open dialogs of each binding that is gone or
// ended, except the bindings this tick served (judged live at most
// machineScanInterval ago, so one that just ended is dropped a tick later),
// edits their posts, and removes the binding's dialogs once their
// seams left (seamsLeft). A machine
// agent does this for every binding, a pinned agent for its own.
func (s *Server) dropGoneDialogs(served map[string]bool, now time.Time) {
	root, err := dialog.Root()
	if err != nil {
		return
	}
	var names []string
	if s.cfg.Machine {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
	} else if s.cfg.Pin != "" {
		names = []string{s.cfg.Pin}
	}
	snap := s.snapshot()
	for _, name := range names {
		if served[name] {
			continue
		}
		delete(s.dialogs.quiet, name)
		dir := filepath.Join(root, name)
		if _, err := os.Lstat(dir); err != nil {
			continue
		}
		// The snapshot only lets a live binding be skipped: whether it is
		// gone or ended is decided from a fresh read, since a session can
		// attach again while an earlier name's relay call runs.
		if b, ok := snap.binding(name); ok && s.live(b) {
			continue
		}
		b, err := binding.ReadNamed(name)
		switch {
		// A name no binding can have serves nothing.
		case errors.Is(err, binding.ErrNone), binding.ValidName(name) != nil:
		case err == nil && !s.live(b):
		default:
			continue
		}
		reqs, _ := dialog.Requests(dir)
		for _, req := range reqs {
			_, _ = dialog.Resolve(dir, dialog.Resolved{ID: req.ID, Outcome: dialog.OutcomeExpired, By: dialog.ByTimeout, At: now.UTC()})
		}
		s.dialogClose(name, dir, nil, now)
		if seamsLeft(dir, now) {
			_ = os.RemoveAll(dir)
			_ = os.RemoveAll(s.dialogPostDir(name))
		}
	}
}

// seamGrace is how long a resolved dialog's directory stays while its seam
// process still runs, so the seam reads how its dialog ended.
const seamGrace = 2 * time.Minute

// seamsLeft reports whether every dialog in dir is resolved and its seam has
// read that or cannot any more: its process is gone, or the resolution is
// older than seamGrace. Removing the directory earlier would let a seam that
// still waits write into a directory that comes back.
func seamsLeft(dir string, now time.Time) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "requests"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		res, resolved, err := dialog.ReadResolved(dir, id)
		if err != nil || !resolved {
			return false
		}
		var req dialog.Request
		if raw, err := readSmallRegular(filepath.Join(dir, "requests", e.Name())); err == nil && json.Unmarshal(raw, &req) == nil &&
			req.PID > 0 && pidAlive(req.PID) && now.Sub(res.At) < seamGrace {
			return false
		}
	}
	return true
}

// pruneDialogs removes the files of each dialog resolved more than
// dialogKeep ago, the request first, so it never reads as open again. due
// is when the next kept dialog is to be removed; it is now when one could
// not be read or removed.
func (s *Server) pruneDialogs(name, dir string, now time.Time) (due time.Time) {
	due = now.Add(dialogKeep)
	entries, _ := os.ReadDir(filepath.Join(dir, "resolved"))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		res, found, err := dialog.ReadResolved(dir, id)
		if err != nil || !found {
			due = now
			continue
		}
		if now.Sub(res.At) <= dialogKeep {
			due = earliest(due, res.At.Add(dialogKeep))
			continue
		}
		if os.Remove(filepath.Join(dir, "requests", e.Name())) != nil && !notExist(filepath.Join(dir, "requests", e.Name())) {
			due = now
			continue
		}
		_ = os.Remove(filepath.Join(dir, "answers", e.Name()))
		_ = os.Remove(filepath.Join(s.dialogPostDir(name), e.Name()))
		_ = os.Remove(filepath.Join(s.dialogPostDir(name), "done", e.Name()))
		_ = os.Remove(filepath.Join(dir, "resolved", e.Name()))
		delete(s.dialogs.polled, id)
		delete(s.dialogs.edits, id)
	}
	return due
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func notExist(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func (s *Server) dialogPostDir(name string) string {
	return filepath.Join(s.cfg.StateDir, "dialogs", name)
}

// dialogStep posts req once, then reads the owner's reactions on the post
// every reactionPollGap until it is answered: ❌ picks the deny option and
// beats ✅, which picks option 1.
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
	if phase, _, _, _, err := dialog.Lookup(dir, req.ID); err != nil || phase != dialog.PhaseOpen {
		return
	}
	if s.dialogs.polled == nil {
		s.dialogs.polled = map[string]time.Time{}
	}
	if last, ok := s.dialogs.polled[req.ID]; ok && now.Sub(last) < reactionPollGap(now.Sub(req.OpenedAt)) {
		return
	}
	s.dialogs.polled[req.ID] = now
	rs, err := readReactions(p.Event, s.cfg.PostTimeout)
	if err != nil {
		return
	}
	if n := reactionOption(req, rs); n != 0 {
		_, _ = dialog.WriteAnswer(dir, dialog.Answer{ID: req.ID, Option: n, By: dialog.ByBuzz, At: now.UTC()})
	}
}

// reactionOption is the option the owner's reaction picks: ❌ the deny
// option, which beats ✅, option 1. A reaction never answers a dialog
// without that option; 0 means no answer.
func reactionOption(req dialog.Request, rs []reaction) int {
	if reacted(rs, buzzOwner, "❌") {
		for _, o := range req.Options {
			if o.Deny {
				return o.N
			}
		}
	}
	if reacted(rs, buzzOwner, "✅") {
		for _, o := range req.Options {
			if o.N == 1 {
				return 1
			}
		}
	}
	return 0
}

// postDialog claims req's post record, posts it and records the message. A
// post proved not sent drops the claim so the next tick posts again; any
// other failure leaves the outcome unknown, and the dialog is not posted
// again.
func (s *Server) postDialog(path string, t dialogTarget, req dialog.Request) {
	label := t.label
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
	_ = s.writeDialogPost(path, p)
}

// dialogClose edits the post of each dialog that is no longer open and has
// a resolution with its outcome, and moves its record to done/. A failed
// edit is tried again after a backoff (dialogEditBackoffMin, doubling).
// closed reports whether no post is left to edit.
func (s *Server) dialogClose(name, dir string, open map[string]bool, now time.Time) (closed bool) {
	postDir := s.dialogPostDir(name)
	entries, _ := os.ReadDir(postDir)
	left := 0
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		left++
		if open[id] {
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
		if bo, ok := s.dialogs.edits[id]; ok && now.Before(bo.until) {
			continue
		}
		if p.Event != "" && editMessage(p.Event, p.Text+"\n\n"+outcomeLine(res), s.cfg.PostTimeout) != nil {
			s.dialogEditBackOff(id, now)
			continue
		}
		delete(s.dialogs.polled, id)
		delete(s.dialogs.edits, id)
		done := filepath.Join(postDir, "done")
		if os.MkdirAll(done, 0o700) == nil && os.Rename(path, filepath.Join(done, e.Name())) == nil {
			left--
		}
	}
	return left == 0
}

// dialogEditBackOff delays the next edit of dialog id's post.
func (s *Server) dialogEditBackOff(id string, now time.Time) {
	if s.dialogs.edits == nil {
		s.dialogs.edits = map[string]mirrorBackoff{}
	}
	bo := s.dialogs.edits[id]
	bo.step = min(max(bo.step*2, dialogEditBackoffMin), dialogEditBackoffMax)
	bo.until = now.Add(bo.step)
	s.dialogs.edits[id] = bo
}

// DialogPosted reports whether the Buzz agent of binding name posted dialog
// id and knows the posted message, so the owner can answer it in Buzz. It is
// false while the dialog is unposted, while its post is in flight, and when
// the post's outcome is unknown.
func DialogPosted(name, id string) bool {
	stateDir, err := BindingStateDir()
	if err != nil {
		return false
	}
	p, found := readDialogPost(filepath.Join(stateDir, "dialogs", name, id+".json"))
	return found && p.Event != ""
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

func (s *Server) writeDialogPost(path string, p dialogPost) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.writeState(path, raw)
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
	// A blank line ends the option list, so Buzz shows the hint as its own
	// paragraph, not as part of the last option.
	b.WriteString("\n" + strings.Join(how, ", ") + ".")
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

// dialogPin is the marker directory name whose dialog lock elects the
// worker that posts dialogs: the machine agent's, or a pinned agent's pin;
// "" when this agent posts none.
func (s *Server) dialogPin() string {
	switch {
	case s.cfg.Machine:
		return machineName
	case !s.cfg.Helper && s.cfg.Pin != "" && s.cfg.PinErr == nil:
		return s.cfg.Pin
	}
	return ""
}

// dialogLead reports whether this worker posts its agent's dialogs: the
// worker that holds <dialogPin>/<self>.dialog.lock.
func (s *Server) dialogLead() bool {
	if s.dialogs.release != nil || !lock.AdvisoryLockAvailable() {
		return true
	}
	if buzzSelf == "" {
		return false
	}
	dir := s.agentMarkerDir(s.dialogPin())
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

// dialogReply returns the answer that the owner's Buzz message in channel
// gives to an open dialog posted there, and the dialog's directory. The
// prompt text only selects a message that looks like an answer; the answer
// itself is the verified owner event (ownerEvent): its content is the answer
// and its e tags are its thread. A thread reply to a dialog's post answers
// that dialog; any other message answers only when exactly one dialog is
// open in channel. With no verified event the dialogs stay pending and the
// channel is told why, once per open set.
func (s *Server) dialogReply(channel, eventID, prompt string) (string, dialog.Answer, bool) {
	if channel == "" || buzzOwner == "" {
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
	dir, err := dialog.DirFor(name)
	if err != nil {
		return "", dialog.Answer{}, false
	}
	reqs, err := dialog.Requests(dir)
	if err != nil {
		return "", dialog.Answer{}, false
	}
	var open []dialog.Request
	var openIDs []string
	posted := map[string]string{}
	for _, req := range reqs {
		p, found := readDialogPost(filepath.Join(s.dialogPostDir(name), req.ID+".json"))
		if !found || p.Channel != channel {
			continue
		}
		open, openIDs, posted[req.ID] = append(open, req), append(openIDs, req.ID), p.Event
	}
	if len(open) == 0 || !slices.ContainsFunc(open, func(req dialog.Request) bool {
		_, ok := dialog.ParseAnswer(req, ownerWords(prompt))
		return ok
	}) {
		return "", dialog.Answer{}, false
	}
	evt, err := ownerEvent(eventID, prompt, channel)
	if err != nil {
		s.tellUnverified(channel, "dialog:"+strings.Join(openIDs, ","), "Your reply was not taken as an answer", err)
		return "", dialog.Answer{}, false
	}
	req, ok := answerTarget(dir, evt, open, posted, time.Now())
	if !ok {
		return "", dialog.Answer{}, false
	}
	a, ok := dialog.ParseAnswer(req, dropMentions(evt.Content))
	a.Event = evt.ID.Hex()
	return dir, a, ok
}

// answerTarget is the open dialog that the verified owner event evt
// answers: the one dialog whose post its NIP-10 root or reply names, or,
// when it has no e tag at all, the only dialog open in its channel. It
// answers nothing when its refs name no open dialog or more than one, when
// it fails the time rule against its dialog's opening (signedWithin), or
// when it is bound to another dialog. Choosing binds evt to its dialog
// (BindEvent) before any answer is written, so no later path moves it.
// posted maps each open dialog to its post's event id.
func answerTarget(dir string, evt nostr.Event, open []dialog.Request, posted map[string]string, now time.Time) (dialog.Request, bool) {
	refs, tagged := replyRefs(evt)
	var here []dialog.Request
	for _, req := range open {
		if !tagged || posted[req.ID] != "" && slices.Contains(refs, posted[req.ID]) {
			here = append(here, req)
		}
	}
	if len(here) != 1 || !signedWithin(evt, here[0].OpenedAt.Add(-ownerSkew), now) {
		return dialog.Request{}, false
	}
	if bound, err := dialog.BindEvent(dir, evt.ID.Hex(), here[0].ID); err != nil || bound != here[0].ID {
		return dialog.Request{}, false
	}
	return here[0], true
}

// ownerSkew bounds how far the clock that dates an owner's signed message
// may be from this machine's.
const ownerSkew = 60 * time.Second

// signedWithin is the time rule of every owner decision: evt was signed no
// earlier than floor and at most ownerSkew after now. A replayed message is
// older than the state it would decide, and a message dated ahead would
// outrank the owner's later ones.
func signedWithin(evt nostr.Event, floor, now time.Time) bool {
	at := evt.CreatedAt.Time()
	return !at.Before(floor) && !at.After(now.Add(ownerSkew))
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

// ownerWords is the text a buzz-acp prompt shows the owner typed: the
// Content of its one Buzz event, else what follows the <context> block, with
// leading mentions dropped. It only says whether a prompt may be an owner
// decision; the decision itself reads the verified event (ownerEvent).
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
	return dropMentions(text)
}

// dropMentions trims text and drops its leading mentions. Line breaks stay:
// one line can answer each question of a dialog.
func dropMentions(text string) string {
	text = strings.TrimSpace(text)
	for {
		end := strings.IndexFunc(text, unicode.IsSpace)
		if end < 0 || (!strings.HasPrefix(text, "@") && !strings.HasPrefix(text, "nostr:")) {
			return text
		}
		text = strings.TrimSpace(text[end:])
	}
}

// errNotOwner is a verified event that someone other than the owner signed.
var errNotOwner = errors.New("the event is not the owner's")

// errNoEventID is a prompt that names no Nostr event to verify.
var errNoEventID = errors.New("the message names no signed event")

// ownerEvent is the one source of an owner decision: the prompt's
// triggering event (lookupEventID of its _meta id eventID and its text) as
// the relay serves it to this agent, accepted by buzzevent.Fetch as the
// plaintext message in channel, and signed by the owner. A decision acts on
// its content and tags, never on the prompt text. errNotOwner means it
// verified but another key signed it; any other error means there is no
// verified event.
func ownerEvent(eventID, prompt, channel string) (nostr.Event, error) {
	id := lookupEventID(eventID, prompt)
	if id == "" {
		return nostr.Event{}, errNoEventID
	}
	evt, err := buzzevent.Fetch(context.Background(), buzzRelay, id, channel)
	if err != nil {
		return nostr.Event{}, err
	}
	if buzzOwner == "" || evt.PubKey.Hex() != buzzOwner {
		return nostr.Event{}, errNotOwner
	}
	return evt, nil
}

// unverifiedNoticeEvery is how often one decision may say why it was not
// taken.
const unverifiedNoticeEvery = 10 * time.Minute

// noticeLimit lets one line per key through every unverifiedNoticeEvery.
type noticeLimit struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (n *noticeLimit) allow(key string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, at := range n.last {
		if now.Sub(at) >= unverifiedNoticeEvery {
			delete(n.last, k)
		}
	}
	if _, recent := n.last[key]; recent {
		return false
	}
	if n.last == nil {
		n.last = map[string]time.Time{}
	}
	n.last[key] = now
	return true
}

// tellUnverified posts one line into channel saying why an owner decision
// was not taken, at most once per key every unverifiedNoticeEvery. A
// verified event from someone other than the owner says nothing.
func (s *Server) tellUnverified(channel, key, lead string, err error) {
	if errors.Is(err, errNotOwner) {
		return
	}
	fmt.Fprintf(os.Stderr, "amq-acp: an owner decision in %s was not taken: %v\n", channel, err)
	if !s.notices.allow(key, time.Now()) {
		return
	}
	var why string
	switch {
	case errors.Is(err, errNoEventID):
		why = err.Error()
	case errors.Is(err, buzzevent.ErrNotFound):
		why = "the relay does not serve its signed event"
	case errors.Is(err, buzzevent.ErrWrongID), errors.Is(err, buzzevent.ErrWrongKind), errors.Is(err, buzzevent.ErrWrongChannel):
		why = "its signed event is not a message in this channel"
	default:
		why = "its signed event could not be read from the relay (" + relay.Category(err) + ")"
	}
	go s.publish(channel, lead+": "+why+".")
}

// replyRefs are the events a signed message replies to under NIP-10: its e
// tags marked "root" or "reply", or, when no e tag has a marker, its first
// and last e tag. A "mention" never counts. tagged reports whether the
// message has any e tag.
func replyRefs(evt nostr.Event) (refs []string, tagged bool) {
	var es []nostr.Tag
	marked := false
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == "e" {
			es = append(es, t)
			marked = marked || len(t) >= 4 && t[3] != ""
		}
	}
	for i, t := range es {
		reply := len(t) >= 4 && (t[3] == "root" || t[3] == "reply")
		if !marked {
			reply = i == 0 || i == len(es)-1
		}
		if reply && nostrEventIDRe.MatchString(t[1]) && !slices.Contains(refs, t[1]) {
			refs = append(refs, t[1])
		}
	}
	return refs, len(es) > 0
}
