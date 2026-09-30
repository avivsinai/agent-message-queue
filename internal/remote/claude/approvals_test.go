package claude

import (
	"bytes"
	"encoding/json"
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
	in, _ := json.Marshal(map[string]any{
		"session_id": approvalSession, "prompt_id": "p-1", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": command},
	})
	h := permissionHook{home: f.home, now: func() time.Time { return f.base }, wait: time.Minute, poll: 5 * time.Millisecond, markerGrace: 0}
	go func() {
		defer close(f.exited)
		h.run(bytes.NewReader(in), &f.out, f.done)
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
	if q := f.question(); q.ApproveOption != optionAllow || !q.RemoteAnswer {
		t.Fatalf("question = %+v, want approve offered for a Bash command shown whole", q)
	}
	close(f.done)
	f.hookExited()
	if f.out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", f.out.String())
	}
	if ev := f.resolution(); ev.Outcome != protocol.ResolutionElsewhere || ev.Remote {
		t.Fatalf("resolution = %+v, want answered_elsewhere", ev)
	}
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionAllow); err != nil || code != protocol.CodeAlreadyResolved {
		t.Fatalf("late approve = %q, %v; want already_resolved", code, err)
	}
}

// Live probe 2026-09-30: a terminal approve does not signal the hook. The
// tool_result of the bound tool_use closes the approval as
// answered_elsewhere, the hook exits with no decision, and a later ✅ is
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
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionAllow); err != nil || code != protocol.CodeAlreadyResolved {
		t.Fatalf("late approve = %q, %v; want already_resolved", code, err)
	}
}

// Ruling on design 9.7 (lead, 2026-09-30): two unanswered identical calls
// in one prompt make the approval uncertain. Approve is not offered, and
// the first tool_result among them closes it as answered_elsewhere.
func TestApprovalTwoIdenticalCallsAreUncertain(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "make build"), bashUse("toolu_2", "make build"))
	f.raise("make build")
	if q := f.question(); q.ApproveOption != "" || len(q.Options) != 1 || q.Options[0] != optionDeny {
		t.Fatalf("question = %+v, want reject only", q)
	}
	if code, _ := f.att.Respond(pr2Key(), "", f.id, optionAllow); code != protocol.CodeInvalid {
		t.Fatalf("approve on an uncertain approval = %q, want invalid", code)
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

// Lead review of bead 611.42.3: an approval not bound to a tool_use is
// reject-only, because a terminal approve of an unbound call is never
// detected. Once the matching tool_use appears, approve is offered.
func TestApprovalOffersApproveOnlyOnceBound(t *testing.T) {
	f := newApprovalFixture(t)
	f.raise("go test ./...")
	if q := f.question(); q.ApproveOption != "" || len(q.Options) != 1 || q.Options[0] != optionDeny {
		t.Fatalf("unbound question = %+v, want reject only", q)
	}
	f.append(map[string]any{
		"type": "assistant", "timestamp": f.stamp(0),
		"message": map[string]any{"role": "assistant", "content": []map[string]any{bashUse("toolu_1", "go test ./...")}},
	})
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		f.att.pollConfirmations()
		f.mu.Lock()
		offered := false
		for _, ev := range f.events {
			offered = offered || ev.Type == core.EventQuestion && ev.Interaction != nil && ev.Interaction.InteractionID == f.id && ev.Interaction.ApproveOption == optionAllow
		}
		f.mu.Unlock()
		if offered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("approve was never offered after the matching tool_use appeared")
		}
	}
	close(f.done)
	f.hookExited()
}
