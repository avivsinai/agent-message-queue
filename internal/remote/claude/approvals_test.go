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
}

const approvalSession = "sess-abc"

func newApprovalFixture(t *testing.T, toolUses ...map[string]any) *approvalFixture {
	t.Helper()
	ft := newFakeTarget(t, 4242, nil)
	att, err := Attach(config{Pid: 4242, Home: ft.home, Target: "cc-1", Approve: true})
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
	if _, err := PinApprovals(f.home, approvalSession, "share-1", os.Getpid()); err != nil {
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
func (f *approvalFixture) raiseWith(command string, ticks <-chan time.Time) {
	f.t.Helper()
	in, _ := json.Marshal(map[string]any{
		"session_id": approvalSession, "prompt_id": "p-1", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": command},
	})
	h := permissionHook{home: f.home, now: func() time.Time { return f.base }, wait: time.Minute, poll: 5 * time.Millisecond, markerGrace: 0, ticks: ticks}
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

// Live probe 2026-09-30: a terminal approve does not signal the hook. The
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

// Ruling on design 9.7 (lead, 2026-09-30): two unanswered identical calls
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
	// Owner ruling on bead 611.42.3: Buzz can only block a Claude tool call.
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

// Lead review of bead 611.42.3: an approval binds to its tool_use when that
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

// Pro review of #929, 2026-09-30, #1: an answer file waiting on disk is not
// delivery. The terminal approved, the tool_result appeared and a Buzz answer
// landed before the hook read it: the terminal's closure wins the resolved
// file, and the hook prints nothing.
func TestApprovalTerminalApproveBeatsPendingBuzzAnswer(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	ticks := make(chan time.Time)
	f.raiseWith("go test ./...", ticks)
	f.question()
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionDeny); err != nil || code != "" {
		t.Fatalf("respond = %q, %v; want the answer written", code, err)
	}
	f.append(toolResult(f, "toolu_1", 1))
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionElsewhere || ev.Remote {
		t.Fatalf("resolution = %+v, want answered_elsewhere", ev)
	}
	ticks <- time.Time{} // the hook now reads the answer
	f.hookExited()
	if f.out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", f.out.String())
	}
	if r := f.resolvedOnDisk(); r.Outcome != protocol.ResolutionElsewhere {
		t.Fatalf("resolved = %+v, want answered_elsewhere", r)
	}
}

// Pro review of #929, 2026-09-30, #2: two identical calls split across the
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

// Pro review of #929, 2026-09-30, #3: a tool_result and its Stop marker in
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

// Pro review of #929 r2, 2026-10-01, #1: a request found after a transcript
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

// Pro review of #929 r2, 2026-10-01, #2, and r3, 2026-10-01, #1: the
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
// with pid hookPID leaves them, plus its delivery record when delivery is
// not nil.
func (f *approvalFixture) writeClaim(hookPID int, delivery *bool) {
	f.t.Helper()
	f.writeRequest(hookPID)
	if err := writeResolved(f.home, approvalSession, approvalResolved{InteractionID: f.id, Outcome: outcomeHookClaim, Option: optionDeny}); err != nil {
		f.t.Fatal(err)
	}
	if delivery != nil {
		if err := writeDelivery(f.home, approvalSession, f.id, *delivery); err != nil {
			f.t.Fatal(err)
		}
	}
}

// writeRequest lays down a request a hook with pid hookPID raised.
func (f *approvalFixture) writeRequest(hookPID int) {
	f.t.Helper()
	hash, _ := actionHash("Bash", json.RawMessage(`{"command":"go test ./..."}`))
	id, _ := newInteractionID()
	f.id = id
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

// Pro review of #929 r5, 2026-10-01, #1: the registry moves to session B
// while an approval of session A is open. The hook writes its deny and its
// delivery record in A and exits. The approval settles from A's files as
// answered, and nothing is arbitrated into B's directory.
func TestApprovalSettlesInItsOwnSessionAfterASwitch(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.writeRequest(os.Getpid())
	f.question()
	regPath := filepath.Join(claudeSessionsDir(f.home), "4242.json")
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

// Pro review of #929 r3, 2026-10-01, #1: an unfinished claim is delivery
// unknown. A hook stopped before its stdout write and one stopped after it
// but before its delivery record leave the same files, a claim and no
// record; a hook whose record write failed leaves them too. With the hook
// gone, each settles delivery_unknown, as does a record of a failed write.
func TestApprovalUnfinishedClaimIsDeliveryUnknown(t *testing.T) {
	notWritten := false
	for _, tc := range []struct {
		name     string
		delivery *bool
	}{
		{"interrupted before stdout", nil},
		{"interrupted after stdout, before the record", nil},
		{"delivery record write failed", nil},
		{"stdout write failed", &notWritten},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
			f.writeClaim(0, tc.delivery) // pid 0: no live hook
			if ev := f.resolution(); ev.Outcome != protocol.ResolutionDeliveryUnknown || ev.Remote {
				t.Fatalf("resolution = %+v, want delivery_unknown", ev)
			}
		})
	}
}

// Pro review of #929 r3 and r4, 2026-10-01, #1 and #2: the run ends while
// a live hook's claim has no delivery record. The run's end records
// delivery_unknown at once and stops tracking it; a record the hook writes
// later loses to the one the run's end wrote.
func TestApprovalRunEndBeforeDeliveryRecordsDeliveryUnknown(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	f.writeClaim(os.Getpid(), nil)
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
