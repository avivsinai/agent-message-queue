package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// approvalFixture is one AMQ run of a pinned session whose delivery line
// carries promptId p-1, with the hook driven as a function on an injected
// clock and the poller driven directly.
type approvalFixture struct {
	t      *testing.T
	home   string
	att    *Attachment
	base   time.Time
	mu     sync.Mutex
	events []core.NativeEvent
	out    bytes.Buffer
	// stdout replaces out as the hook's stdout when set.
	stdout io.Writer
	done   chan struct{}
	exited chan struct{}
	id     string
	// hookNow and allow, when set, are the hook's clock and allow config.
	hookNow func() time.Time
	allow   AllowConfig
}

const approvalSession = "sess-abc"

func newApprovalFixture(t *testing.T, toolUses ...map[string]any) *approvalFixture {
	t.Helper()
	return newPinnedFixture(t, false, toolUses...)
}

// newPinnedFixture is newApprovalFixture with an answering pin and approve,
// or, when observe is set, an observe pin and observe_approvals.
func newPinnedFixture(t *testing.T, observe bool, toolUses ...map[string]any) *approvalFixture {
	t.Helper()
	ft := newFakeTarget(t, 4242, nil)
	att, err := Attach(config{Pid: 4242, Home: ft.home, Target: "cc-1", Approve: !observe, ObserveApprovals: observe})
	if err != nil {
		t.Fatal(err)
	}
	f := &approvalFixture{t: t, home: ft.home, att: att, base: time.Now().Truncate(time.Second), done: make(chan struct{}), exited: make(chan struct{})}
	att.SetNow(func() time.Time { return f.base })
	unsub := att.Subscribe(func(ev core.NativeEvent) {
		f.mu.Lock()
		f.events = append(f.events, ev)
		f.mu.Unlock()
	})
	t.Cleanup(unsub)
	adm, err := att.Submit(pr2BoundRequest("run the tests"))
	if err != nil || !adm.Admitted {
		t.Fatalf("submit = %+v, %v", adm, err)
	}
	waitRecv(t, ft)
	pin := func() (string, error) { return PinApprovals(f.home, approvalSession, "share-1", "", os.Getpid()) }
	if observe {
		pin = func() (string, error) { return ObserveApprovals(f.home, approvalSession, os.Getpid()) }
	}
	if _, err := pin(); err != nil {
		t.Fatal(err)
	}
	f.append(map[string]any{
		"type": "user", "promptId": "p-1", "timestamp": f.stamp(0),
		"message": map[string]any{"role": "user", "content": harnessUserText("<cross-session-message>run the tests</cross-session-message>")},
		"origin":  map[string]any{"kind": "peer", "msg_id": adm.RunID},
	}, map[string]any{
		"type": "assistant", "timestamp": f.stamp(0),
		"message": map[string]any{"role": "assistant", "content": toolUses},
	})
	att.pollConfirmations()
	if !runMarked(f.home, approvalSession, "p-1") {
		t.Fatal("runs/p-1 was not written for the run's own delivery")
	}
	return f
}

func bashUse(id, command string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}
}

func (f *approvalFixture) stamp(sec int) string {
	return f.base.Add(time.Duration(sec) * time.Second).UTC().Format(time.RFC3339Nano)
}

func (f *approvalFixture) append(lines ...map[string]any) {
	f.t.Helper()
	var raw []string
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = append(raw, string(b))
	}
	appendTranscript(f.t, f.home, raw...)
}

// raise runs the hook for a Bash call and waits, bounded, for its request.
func (f *approvalFixture) raise(command string) {
	f.t.Helper()
	f.raiseWith(command, nil)
}

// raiseWith is raise with the hook's poll paced by ticks, nil for its own
// ticker.
func (f *approvalFixture) raiseWith(command string, ticks chan time.Time) {
	f.t.Helper()
	in, _ := json.Marshal(map[string]any{
		"session_id": approvalSession, "prompt_id": "p-1", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": command},
	})
	h := permissionHook{home: f.home, now: func() time.Time { return f.base }, wait: time.Minute, poll: 5 * time.Millisecond, markerGrace: 0, ticks: ticks, allow: f.allow}
	if f.hookNow != nil {
		h.now = f.hookNow
	}
	var stdout io.Writer = &f.out
	if f.stdout != nil {
		stdout = f.stdout
	}
	go func() {
		defer close(f.exited)
		h.run(bytes.NewReader(in), stdout, f.done)
	}()
	dir := filepath.Join(approveDir(f.home, approvalSession), "requests")
	for deadline := time.Now().Add(3 * time.Second); f.id == ""; time.Sleep(time.Millisecond) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && interactionIDRe.MatchString(id) {
				f.id = id
			}
		}
		if time.Now().After(deadline) {
			f.t.Fatal("the hook raised no request")
		}
	}
	if ticks != nil {
		// The request is visible at its link, before the hook's directory
		// sync returns; a tick taken means the hook reached its poll loop.
		select {
		case ticks <- time.Now():
		case <-f.exited:
		}
	}
	f.att.pollConfirmations()
}

func (f *approvalFixture) question() *protocol.Interaction {
	f.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		for _, ev := range f.events {
			if ev.Type == core.EventQuestion && ev.Interaction != nil && ev.Interaction.InteractionID == f.id {
				f.mu.Unlock()
				return ev.Interaction
			}
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			f.t.Fatalf("no question event for %s", f.id)
		}
		f.att.pollConfirmations()
	}
}

// resolution polls until the approval closes and returns its event.
func (f *approvalFixture) resolution() core.NativeEvent {
	f.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		for _, ev := range f.events {
			if ev.Type == core.EventQuestionResolved && ev.Interaction != nil && ev.Interaction.InteractionID == f.id {
				f.mu.Unlock()
				return ev
			}
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			f.t.Fatalf("approval %s never resolved", f.id)
		}
		f.att.pollConfirmations()
	}
}

func (f *approvalFixture) hookExited() {
	f.t.Helper()
	select {
	case <-f.exited:
	case <-time.After(3 * time.Second):
		f.t.Fatal("the hook did not exit")
	}
}

// Design section 7, terminal first: a terminal reject sends TERM to the
// hook. The hook prints no decision and records answered_elsewhere, and a
// later ✅ is refused already_resolved.
func TestApprovalTerminalRejectFirst(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.raise("go test ./...")
	if q := f.question(); q.ApproveOption != "" || len(q.Options) != 1 || q.Options[0] != optionDeny || !q.RemoteAnswer {
		t.Fatalf("question = %+v, want a remote reject only", q)
	}
	close(f.done)
	f.hookExited()
	if f.out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", f.out.String())
	}
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionElsewhere || ev.Remote {
		t.Fatalf("resolution = %+v, want answered_elsewhere", ev)
	}
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionDeny); err != nil || code != protocol.CodeAlreadyResolved {
		t.Fatalf("late reject = %q, %v; want already_resolved", code, err)
	}
}

// An observe-only session (agent-message-queue-9dx.7): the run's snapshot
// shows the pending approval, no remote source can answer it, and the hook
// applies no answer file.
func TestApprovalObserveOnlyShowsAndRefusesAnswers(t *testing.T) {
	f := newPinnedFixture(t, true, bashUse("toolu_1", "go test ./..."))
	ticks := make(chan time.Time)
	f.raiseWith("go test ./...", ticks)
	if q := f.question(); q.RemoteAnswer || q.ApproveOption != "" || q.Prompt == "" {
		t.Fatalf("question = %+v, want the prompt shown with no remote answer", q)
	}
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionDeny); err != nil || code != protocol.CodeUnsupported {
		t.Fatalf("remote reject = %q, %v; want unsupported", code, err)
	}
	dir, err := ensureApproveSubdir(f.home, approvalSession, "answers")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := actionHash("Bash", json.RawMessage(`{"command":"go test ./..."}`))
	if err := createNewJSON(dir, f.id+".json", approvalAnswer{InteractionID: f.id, ActionHash: hash, Option: optionDeny}); err != nil {
		t.Fatal(err)
	}
	// Each tick the hook takes is one more pass over the answer file; the
	// second is taken only by a hook that read it and decided nothing.
	for range 2 {
		select {
		case ticks <- time.Now():
		case <-f.exited:
			t.Fatalf("the hook ended on the answer file; printed %q", f.out.String())
		case <-time.After(3 * time.Second):
			t.Fatal("the hook stopped polling")
		}
	}
	close(f.done)
	f.hookExited()
	if f.out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", f.out.String())
	}
}

// Observed live: a terminal approve does not signal the hook. The
// tool_result of the bound tool_use closes the approval as
// answered_elsewhere, the hook exits with no decision, and a later ❌ is
// refused.
func TestApprovalTerminalApproveFromToolResult(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.raise("go test ./...")
	f.question()
	f.append(map[string]any{
		"type": "user", "promptId": "p-1", "timestamp": f.stamp(1),
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}},
	})
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionElsewhere || ev.Remote {
		t.Fatalf("resolution = %+v, want answered_elsewhere", ev)
	}
	f.hookExited()
	if f.out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", f.out.String())
	}
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionDeny); err != nil || code != protocol.CodeAlreadyResolved {
		t.Fatalf("late reject = %q, %v; want already_resolved", code, err)
	}
}

// binding is the approval's bound tool_use id and whether it is uncertain.
func (f *approvalFixture) binding() (string, bool) {
	f.att.mu.Lock()
	defer f.att.mu.Unlock()
	for _, rec := range f.att.runs {
		if ap := rec.approvals[f.id]; ap != nil {
			return ap.toolUseID, ap.uncertain
		}
	}
	f.t.Fatalf("no approval %s", f.id)
	return "", false
}

// Design rule: two unanswered identical calls
// in one prompt make the approval uncertain: it binds to neither, and the
// first tool_result among them closes it as answered_elsewhere.
func TestApprovalTwoIdenticalCallsAreUncertain(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "make build"), bashUse("toolu_2", "make build"))
	f.raise("make build")
	if q := f.question(); q.ApproveOption != "" || len(q.Options) != 1 || q.Options[0] != optionDeny {
		t.Fatalf("question = %+v, want reject only", q)
	}
	if id, uncertain := f.binding(); id != "" || !uncertain {
		t.Fatalf("binding = %q uncertain %v, want uncertain", id, uncertain)
	}
	// Buzz can only block a Claude tool call.
	if code, _ := f.att.Respond(pr2Key(), "", f.id, "allow"); code != protocol.CodeInvalid {
		t.Fatalf("allow = %q, want invalid", code)
	}
	f.append(map[string]any{
		"type": "user", "promptId": "p-1", "timestamp": f.stamp(1),
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_2", "content": "ok"}}},
	})
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionElsewhere {
		t.Fatalf("resolution = %+v, want answered_elsewhere", ev)
	}
	f.hookExited()
	if f.out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", f.out.String())
	}
}

// Regression: agent-message-queue-611.42.3, an approval binds to its tool_use when that
// line appears after the request opened, and stays reject only.
func TestApprovalBindsWhenItsToolUseAppears(t *testing.T) {
	f := newApprovalFixture(t)
	f.raise("go test ./...")
	if id, _ := f.binding(); id != "" {
		t.Fatalf("bound to %q before any tool_use", id)
	}
	f.append(map[string]any{
		"type": "assistant", "timestamp": f.stamp(0),
		"message": map[string]any{"role": "assistant", "content": []map[string]any{bashUse("toolu_1", "go test ./...")}},
	})
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		f.att.pollConfirmations()
		if id, _ := f.binding(); id == "toolu_1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the approval never bound to its tool_use")
		}
	}
	if q := f.question(); q.ApproveOption != "" || len(q.Options) != 1 {
		t.Fatalf("question = %+v, want reject only", q)
	}
	close(f.done)
	f.hookExited()
}

func toolResult(f *approvalFixture, id string, sec int) map[string]any {
	return map[string]any{
		"type": "user", "promptId": "p-1", "timestamp": f.stamp(sec),
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": "ok"}}},
	}
}

func (f *approvalFixture) resolvedOnDisk() approvalResolved {
	f.t.Helper()
	r, ok := readResolved(f.home, approvalSession, f.id)
	if !ok {
		f.t.Fatalf("no resolved file for %s", f.id)
	}
	return r
}

// Regression: #929, an answer file waiting on disk is not
// delivery. The terminal approved, the tool_result appeared and a Buzz answer
// landed before the hook read it: the terminal's closure wins the resolved
// file, and the hook prints nothing.
//
// The hook is parked inside a poll pass, on its clock, before it reads the
// answer, and released only after the terminal's closure is on disk. A tick
// cannot hold it: the pass a tick starts runs concurrently with the test, and
// in CI run 39592395358 that pass ran late, saw the resolved file and exited,
// so the next unbounded tick send waited for the 10m test timeout.
func TestApprovalTerminalApproveBeatsPendingBuzzAnswer(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	var park atomic.Bool
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	unpark := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unpark)
	f.hookNow = func() time.Time {
		if park.CompareAndSwap(true, false) {
			parked <- struct{}{}
			<-release
		}
		return f.base
	}
	f.raise("go test ./...")
	park.Store(true)
	select {
	case <-parked:
	case <-f.exited:
		t.Fatalf("the hook ended before the answer; printed %q", f.out.String())
	case <-time.After(3 * time.Second):
		t.Fatal("the hook stopped polling")
	}
	f.question()
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionDeny); err != nil || code != "" {
		t.Fatalf("respond = %q, %v; want the answer written", code, err)
	}
	f.append(toolResult(f, "toolu_1", 1))
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionElsewhere || ev.Remote {
		t.Fatalf("resolution = %+v, want answered_elsewhere", ev)
	}
	unpark() // the hook now reads the answer
	f.hookExited()
	if f.out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", f.out.String())
	}
	if r := f.resolvedOnDisk(); r.Outcome != protocol.ResolutionElsewhere {
		t.Fatalf("resolved = %+v, want answered_elsewhere", r)
	}
}

// Regression: #929, two identical calls split across the
// transcript read limit. No binding is decided before the read reaches the
// end of the file, so the approval never binds to the first call alone.
func TestApprovalIdenticalCallsSplitAcrossReadStayUncertain(t *testing.T) {
	f := newApprovalFixture(t)
	f.raise("make build")
	filler := strings.Repeat("x", 1<<20)
	lines := []map[string]any{{"type": "assistant", "timestamp": f.stamp(0),
		"message": map[string]any{"role": "assistant", "content": []map[string]any{bashUse("toolu_1", "make build")}}}}
	for i := 0; i < 5; i++ {
		lines = append(lines, map[string]any{"type": "system", "timestamp": f.stamp(0), "content": filler})
	}
	lines = append(lines, map[string]any{"type": "assistant", "timestamp": f.stamp(0),
		"message": map[string]any{"role": "assistant", "content": []map[string]any{bashUse("toolu_2", "make build")}}})
	f.append(lines...)
	for i := 0; i < 4; i++ {
		f.att.pollConfirmations()
	}
	if id, uncertain := f.binding(); id != "" || !uncertain {
		t.Fatalf("binding = %q uncertain %v, want uncertain", id, uncertain)
	}
	close(f.done)
	f.hookExited()
}

// Regression: #929, a tool_result and its Stop marker in
// one poll. The approval ends once, with the outcome on disk, before the
// run completes.
func TestApprovalToolResultAndStopInOnePollAgree(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.raiseWith("go test ./...", make(chan time.Time))
	f.question()
	f.append(toolResult(f, "toolu_1", 1), map[string]any{"type": "assistant", "timestamp": f.stamp(1),
		"message": map[string]any{"role": "assistant", "content": "done"}})
	appendStopMarker(t, f.home, f.base.Add(2*time.Second).UnixMilli())
	ev := f.resolution()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		completed := false
		for _, e := range f.events {
			completed = completed || e.Type == core.EventRunCompleted
		}
		f.mu.Unlock()
		if completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never completed")
		}
		f.att.pollConfirmations()
	}
	disk := f.resolvedOnDisk()
	kept, ok := f.att.ResolvedInteraction(pr2Key(), "", f.id)
	if !ok || ev.Outcome != disk.Outcome || kept.Outcome != disk.Outcome {
		t.Fatalf("event %s, memory %s (%v), disk %s: want one outcome", ev.Outcome, kept.Outcome, ok, disk.Outcome)
	}
	close(f.done)
	f.hookExited()
}

// Regression: #929, a request found after a transcript
// read must not bind against that read. Call A runs without a permission
// request; an identical call B and its request land between a pass's
// transcript read and its apply. B binds only against a later read, which
// sees both calls, so it is uncertain, and B's tool_result closes it.
func TestApprovalRequestAfterSnapshotBindsOnALaterRead(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_A", "make build"))
	hash, _ := actionHash("Bash", json.RawMessage(`{"command":"make build"}`))
	id, _ := newInteractionID()
	f.id = id
	var once sync.Once
	f.att.mu.Lock()
	f.att.afterTranscriptRead = func() { once.Do(func() { f.writeLateCall(id, hash) }) }
	f.att.mu.Unlock()
	f.att.pollConfirmations() // its transcript read precedes B
	f.question()
	if bound, uncertain := f.binding(); bound == "toolu_A" || bound == "" && !uncertain {
		t.Fatalf("binding = %q uncertain %v, want uncertain or toolu_B", bound, uncertain)
	}
	f.append(toolResult(f, "toolu_B", 1))
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionElsewhere {
		t.Fatalf("resolution = %+v, want answered_elsewhere", ev)
	}
}

// writeLateCall writes call B and its request, as the seam between a
// transcript read and its apply.
func (f *approvalFixture) writeLateCall(id, hash string) {
	f.append(map[string]any{"type": "assistant", "timestamp": f.stamp(0),
		"message": map[string]any{"role": "assistant", "content": []map[string]any{bashUse("toolu_B", "make build")}}})
	dir, err := ensureApproveSubdir(f.home, approvalSession, "requests")
	if err != nil {
		f.t.Error(err)
		return
	}
	req := approvalRequest{Protocol: approvalProtocol, InteractionID: id, SessionID: approvalSession, PromptID: "p-1",
		ToolName: "Bash", Preview: "Bash command:\nmake build", ActionHash: hash, HookPID: os.Getpid(),
		OpenedAt: protocol.FormatTime(f.base), Deadline: protocol.FormatTime(f.base.Add(time.Minute))}
	if err := createNewJSON(dir, id+".json", req); err != nil {
		f.t.Error(err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// Regression: #929, the
// hook's claim on the resolved file is not delivery. When its deny cannot
// be written to stdout, the approval ends delivery_unknown, never as a
// reject sent from Buzz and never as a terminal answer.
func TestApprovalDenyNotWrittenIsNotSent(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.stdout = failWriter{}
	f.raise("go test ./...")
	f.question()
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionDeny); err != nil || code != "" {
		t.Fatalf("respond = %q, %v; want the answer written", code, err)
	}
	f.hookExited()
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionDeliveryUnknown || ev.Remote {
		t.Fatalf("resolution = %+v, want delivery_unknown", ev)
	}
	if res, ok := f.att.ResolvedInteraction(pr2Key(), "", f.id); !ok || res.Outcome != protocol.ResolutionDeliveryUnknown {
		t.Fatalf("kept = %+v (%v), want delivery_unknown", res, ok)
	}
}

// writeClaim lays down a request and the hook's claim on it, as a hook
// with pid hookPID leaves them before its delivery record.
func (f *approvalFixture) writeClaim(hookPID int) {
	f.t.Helper()
	f.id, _ = newInteractionID()
	// The claim goes first: the attachment can settle a dead hook's request
	// as soon as its link makes it visible, before its create returns.
	if err := writeResolved(f.home, approvalSession, approvalResolved{InteractionID: f.id, Outcome: outcomeHookClaim, Option: optionDeny}); err != nil {
		f.t.Fatal(err)
	}
	f.writeRequest(hookPID)
}

// writeRequest lays down a request a hook with pid hookPID raised.
func (f *approvalFixture) writeRequest(hookPID int) {
	f.t.Helper()
	hash, _ := actionHash("Bash", json.RawMessage(`{"command":"go test ./..."}`))
	if f.id == "" {
		f.id, _ = newInteractionID()
	}
	id := f.id
	dir, err := ensureApproveSubdir(f.home, approvalSession, "requests")
	if err != nil {
		f.t.Fatal(err)
	}
	req := approvalRequest{Protocol: approvalProtocol, InteractionID: id, SessionID: approvalSession, PromptID: "p-1",
		ToolName: "Bash", Preview: "Bash command:\ngo test ./...", ActionHash: hash, HookPID: hookPID,
		OpenedAt: protocol.FormatTime(f.base), Deadline: protocol.FormatTime(f.base.Add(time.Minute))}
	if err := createNewJSON(dir, id+".json", req); err != nil {
		f.t.Fatal(err)
	}
}

// Regression: #929, the registry moves to session B
// while an approval of session A is open. The hook writes its deny and its
// delivery record in A and exits. The approval settles from A's files as
// answered, and nothing is arbitrated into B's directory.
func TestApprovalSettlesInItsOwnSessionAfterASwitch(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.writeRequest(os.Getpid())
	f.question()
	regPath := filepath.Join(registryDir(defaultConfigDir(f.home)), "4242.json")
	raw, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]any
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	reg["sessionId"] = "sess-b"
	raw, _ = json.Marshal(reg)
	if err := os.WriteFile(regPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f.att.pollConfirmations() // the poller now follows B
	if err := writeResolved(f.home, approvalSession, approvalResolved{InteractionID: f.id, Outcome: outcomeHookClaim, Option: optionDeny}); err != nil {
		t.Fatal(err)
	}
	if err := writeDelivery(f.home, approvalSession, f.id, true); err != nil {
		t.Fatal(err)
	}
	f.att.mu.Lock()
	for _, rec := range f.att.runs {
		if ap := rec.approvals[f.id]; ap != nil {
			ap.hookPID = 0 // the hook exited
		}
	}
	f.att.mu.Unlock()
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionAnswered || ev.Option != optionDeny {
		t.Fatalf("resolution = %+v, want answered deny from session A's files", ev)
	}
	if r, ok := readResolved(f.home, "sess-b", f.id); ok {
		t.Fatalf("session B holds %+v, want nothing", r)
	}
}

// Regression: #929, an unfinished claim is delivery
// unknown. A hook stopped before its stdout write, one stopped after it but
// before its delivery record, and one whose record write failed all leave
// the same files: a claim and no record. With the hook gone, it settles
// delivery_unknown. A record of a failed write is
// TestApprovalDenyNotWrittenIsNotSent.
func TestApprovalUnfinishedClaimIsDeliveryUnknown(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.writeClaim(0) // pid 0: no live hook
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionDeliveryUnknown || ev.Remote {
		t.Fatalf("resolution = %+v, want delivery_unknown", ev)
	}
}

// Regression: #929, the run ends while
// a live hook's claim has no delivery record. The run's end records
// delivery_unknown at once and stops tracking it; a record the hook writes
// later loses to the one the run's end wrote.
func TestApprovalRunEndBeforeDeliveryRecordsDeliveryUnknown(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.writeClaim(os.Getpid())
	f.question()
	appendStopMarker(t, f.home, f.base.Add(2*time.Second).UnixMilli())
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionDeliveryUnknown || ev.Remote {
		t.Fatalf("resolution = %+v, want delivery_unknown", ev)
	}
	if err := writeDelivery(f.home, approvalSession, f.id, true); err == nil {
		t.Fatal("a late delivery record was accepted after the run's end settled the approval")
	}
	if d, ok := readDelivery(f.home, approvalSession, f.id); !ok || d.Written {
		t.Fatalf("delivery = %+v (%v), want the run end's unwritten record", d, ok)
	}
	if res, ok := f.att.ResolvedInteraction(pr2Key(), "", f.id); !ok || res.Outcome != protocol.ResolutionDeliveryUnknown {
		t.Fatalf("kept = %+v (%v), want delivery_unknown", res, ok)
	}
}

// startHook runs the hook for a Bash call of prompt p-1 in approvalSession
// and returns a channel closed when it exits; closing done ends it.
func startHook(t *testing.T, h permissionHook, done chan struct{}, stdout io.Writer) chan struct{} {
	t.Helper()
	in, _ := json.Marshal(map[string]any{
		"session_id": approvalSession, "prompt_id": "p-1", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "ls"},
	})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		h.run(bytes.NewReader(in), stdout, done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(done)
		}
		<-exited
	})
	return exited
}

// awaitRequest waits, bounded, for the hook's request file.
func awaitRequest(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(approveDir(home, approvalSession), "requests")
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook raised no request")
		}
	}
}

// Regression: hook-audit S2 (bead agent-message-queue-ccl.1), observed on
// every tool dialog of a pinned session: a prompt typed in the terminal
// waited the full 2 s markerGrace for a run marker no AMQ run would write.
// With no run in flight the hook returns at once, on a clock that never
// moves.
func TestApprovalHookDoesNotWaitForATerminalPrompt(t *testing.T) {
	home := t.TempDir()
	if _, err := PinApprovals(home, approvalSession, "share-1", "", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	frozen := time.Now()
	h := permissionHook{home: home, now: func() time.Time { return frozen }, wait: time.Minute, poll: 5 * time.Millisecond, markerGrace: 2 * time.Second}
	var out bytes.Buffer
	select {
	case <-startHook(t, h, make(chan struct{}), &out):
	case <-time.After(time.Second):
		t.Fatal("the hook waited for a run marker with no run in flight")
	}
	if out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", out.String())
	}
	if _, err := os.Stat(filepath.Join(approveDir(home, approvalSession), "requests")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the hook raised a request for a terminal prompt (%v)", err)
	}
}

// Regression: hook-audit S2 review finding (Codex), the reader order. The
// attachment writes the run marker and then removes the run's inflight
// record, both between the hook's missed marker read and its inflight
// read. The hook reads the marker again and raises the request. The hand-off
// runs at the hook's clock read, which comes between those two file reads.
func TestApprovalHookRaisesForARunThatLeftInflightBetweenReads(t *testing.T) {
	home := t.TempDir()
	if _, err := PinApprovals(home, approvalSession, "share-1", "", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	const owner, msgID = "00112233445566ff", "00000000-0000-5000-8000-000000000001"
	if err := writeInflight(home, inflightRecord{Owner: owner, PID: os.Getpid(), SessionID: approvalSession, MsgID: msgID,
		ExpiresAt: protocol.FormatTime(time.Now().Add(time.Minute))}); err != nil {
		t.Fatal(err)
	}
	frozen := time.Now()
	reads := 0
	h := permissionHook{home: home, wait: time.Minute, poll: 5 * time.Millisecond, markerGrace: 2 * time.Second,
		now: func() time.Time {
			// Read 1 starts the grace; read 2 follows the first missed
			// marker read and precedes the inflight read.
			if reads++; reads == 2 {
				if err := writeRunMarker(home, approvalSession, "p-1", runMarker{MsgID: msgID, Owner: owner, PID: os.Getpid()}); err != nil {
					t.Error(err)
				}
				removeInflight(home, approvalSession, owner, msgID)
			}
			return frozen
		}}
	startHook(t, h, make(chan struct{}), io.Discard)
	awaitRequest(t, home)
}

// A run's tool call that asks for permission before the poller claimed the
// run's prompt waits for the claim: Submit records the run in flight
// before its frame, the poll writes runs/p-1 and only then drops the
// inflight record, and the hook raises the request.
func TestApprovalHookWaitsForAnInflightRunsMarker(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att, err := Attach(config{Pid: 4242, Home: ft.home, Target: "cc-1", Approve: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(att.Subscribe(func(core.NativeEvent) {}))
	if _, err := PinApprovals(ft.home, approvalSession, "share-1", "", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	adm, err := att.Submit(pr2BoundRequest("run the tests"))
	if err != nil || !adm.Admitted {
		t.Fatalf("submit = %+v, %v", adm, err)
	}
	waitRecv(t, ft)
	if !inflightLive(ft.home, approvalSession, time.Now()) {
		t.Fatal("the submitted run has no live inflight record")
	}
	// The hook's second clock read follows its first missed marker read.
	missed := make(chan struct{}, 1)
	var reads atomic.Int32
	h := permissionHook{home: ft.home, wait: time.Minute, poll: 5 * time.Millisecond, markerGrace: 2 * time.Second,
		now: func() time.Time {
			if reads.Add(1) == 2 {
				missed <- struct{}{}
			}
			return time.Now()
		}}
	exited := startHook(t, h, make(chan struct{}), io.Discard)
	select {
	case <-missed: // the hook is waiting for the marker
	case <-time.After(3 * time.Second):
		t.Fatal("the hook never looked for the run marker")
	}
	appendTranscript(t, ft.home, mustJSON(t, map[string]any{
		"type": "user", "promptId": "p-1", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": harnessUserText("<cross-session-message>run the tests</cross-session-message>")},
		"origin":  map[string]any{"kind": "peer", "msg_id": adm.RunID},
	}))
	att.pollConfirmations()
	select {
	case <-exited:
		t.Fatal("the hook returned before the run's marker was written")
	default:
	}
	awaitRequest(t, ft.home)
	if inflightLive(ft.home, approvalSession, time.Now()) {
		t.Fatal("the claimed run's inflight record is still live")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
