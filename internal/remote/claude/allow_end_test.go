package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Pro review of #936, P2 (611.42.4): the hook read an allow after its
// deadline and printed it before its timeout check. The hook checks its
// end immediately before and immediately after verifying an allow: an
// allow that verifies is never printed after the deadline, after Claude
// ended the hook, or after the approval was closed.
func TestApprovalHookNeverAllowsAfterItsEnd(t *testing.T) {
	cases := []struct {
		name   string
		before func(f *approvalFixture, clock *atomic.Int64) // runs before the hook reads the answer
		during func(f *approvalFixture, clock *atomic.Int64) // runs inside the verifier
		allows bool
	}{
		{name: "verified in time (control)", allows: true},
		{name: "answer read after the deadline", before: func(_ *approvalFixture, clock *atomic.Int64) {
			clock.Store(int64(2 * time.Minute))
		}},
		{name: "deadline passes during verification", during: func(_ *approvalFixture, clock *atomic.Int64) {
			clock.Store(int64(2 * time.Minute))
		}},
		{name: "Claude ends the hook during verification", during: func(f *approvalFixture, _ *atomic.Int64) {
			close(f.done)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
			var clock atomic.Int64
			f.hookNow = func() time.Time { return f.base.Add(time.Duration(clock.Load())) }
			owner := strings.Repeat("ab", 32)
			f.allow = AllowConfig{
				Owner: owner,
				Share: func(string) (AllowShare, error) {
					return AllowShare{Owner: owner, Body: strings.Repeat("cd", 32), Channel: "dm-1", Target: "cc-1"}, nil
				},
				Verify: func(context.Context, json.RawMessage, AllowCheck) error {
					if tc.during != nil {
						tc.during(f, &clock)
					}
					return nil // the evidence verifies
				},
			}
			ticks := make(chan time.Time)
			f.raiseWith("go test ./...", ticks)
			var req approvalRequest
			if err := readApprovalJSON(filepath.Join(approveDir(f.home, approvalSession), "requests", f.id+".json"), &req); err != nil || !req.Approvable {
				t.Fatalf("request = %+v (%v), want an approvable call", req, err)
			}
			dir, err := ensureApproveSubdir(f.home, approvalSession, "answers")
			if err != nil {
				t.Fatal(err)
			}
			ans, _ := json.Marshal(approvalAnswer{InteractionID: f.id, ActionHash: req.ActionHash, Option: optionAllow, Evidence: json.RawMessage(`{}`)})
			if err := os.WriteFile(filepath.Join(dir, f.id+".json"), ans, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.before != nil {
				tc.before(f, &clock)
			}
			select {
			case ticks <- time.Now():
			case <-f.exited:
			}
			f.hookExited()
			if tc.allows {
				if !strings.Contains(f.out.String(), `"behavior":"allow"`) {
					t.Fatalf("hook printed %q, want allow", f.out.String())
				}
				return
			}
			if f.out.Len() != 0 {
				t.Fatalf("hook printed %q, want no decision", f.out.String())
			}
			if r := f.resolvedOnDisk(); r.Outcome == outcomeHookClaim {
				t.Fatalf("resolved = %+v, want no hook claim", r)
			}
		})
	}
}

// 611.42.6 (independent review of #936 r3): the hook's own verification of
// a ✅ had no bound but Claude's TERM, so an unreachable relay held the hook
// and a ❌ that arrived meanwhile landed only after it. The verifier stands
// in for the relay sign-in: it fails at its deadline, and with none it
// waits as an unreachable relay does.
func TestApprovalHookBoundsItsVerifyAndThenDenies(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	owner := strings.Repeat("ab", 32)
	f.allow = AllowConfig{
		Owner: owner,
		Share: func(string) (AllowShare, error) {
			return AllowShare{Owner: owner, Body: strings.Repeat("cd", 32), Channel: "dm-1", Target: "cc-1"}, nil
		},
		Verify: func(ctx context.Context, _ json.RawMessage, _ AllowCheck) error {
			if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= allowVerifyTimeout {
				return context.DeadlineExceeded
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	ticks := make(chan time.Time)
	f.raiseWith("go test ./...", ticks)
	var req approvalRequest
	if err := readApprovalJSON(filepath.Join(approveDir(f.home, approvalSession), "requests", f.id+".json"), &req); err != nil || !req.Approvable {
		t.Fatalf("request = %+v (%v), want an approvable call", req, err)
	}
	dir, err := ensureApproveSubdir(f.home, approvalSession, "answers")
	if err != nil {
		t.Fatal(err)
	}
	answer := func(option string, evidence json.RawMessage) {
		raw, _ := json.Marshal(approvalAnswer{InteractionID: f.id, ActionHash: req.ActionHash, Option: option, Evidence: evidence})
		if err := os.WriteFile(filepath.Join(dir, f.id+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tick := func() bool {
		select {
		case ticks <- time.Now():
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}
	answer(optionAllow, json.RawMessage(`{}`))
	first := tick()
	if second := tick(); !first || !second { // the second tick waits for the verify to end
		close(f.done)
		f.hookExited()
		t.Fatal("the hook's verify of the ✅ did not end within its bound")
	}
	answer(optionDeny, nil)
	tick()
	f.hookExited()
	if !strings.Contains(f.out.String(), `"behavior":"deny"`) {
		t.Fatalf("hook printed %q, want the later ❌ as deny", f.out.String())
	}
}

// 611.42.6 (independent review of #936 r3): after the hook refused an
// allow, a ✅ and a ❌ arrive at once. Both found the refused allow on disk
// and both replaced it, so the ✅ verified meanwhile overwrote the ❌ the
// endpoint had recorded. The first write now stands and the other answer
// is already_resolved.
func TestApprovalAllowAndDenyAfterARefusedAllowAgree(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	owner := strings.Repeat("ab", 32)
	pin := HookPin{Owner: owner, Root: "/amq-root", Session: "share-1", Relay: "wss://relay.example", Body: strings.Repeat("cd", 32), Channel: "dm-1", Target: "cc-1"}
	if err := InstallPermissionHook(f.home, "/opt/amq-remote", DefaultPermissionWait, pin); err != nil {
		t.Fatal(err)
	}
	if _, err := PinApprovals(f.home, approvalSession, "share-1", owner, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	share := func(string) (AllowShare, error) {
		return AllowShare{Owner: owner, Body: pin.Body, Channel: "dm-1", Target: "cc-1"}, nil
	}
	f.allow = AllowConfig{Owner: owner, Share: share, Verify: func(context.Context, json.RawMessage, AllowCheck) error {
		return context.DeadlineExceeded // the hook's own relay read fails
	}}
	f.att.SetAllowFactory(func(HookPin) AllowConfig {
		return AllowConfig{Owner: owner, Share: share, Verify: func(context.Context, json.RawMessage, AllowCheck) error {
			close(entered)
			<-release
			return nil
		}}
	})
	f.raise("go test ./...")
	if q := f.question(); q.ApproveOption != optionAllow {
		t.Fatalf("question = %+v, want allow offered", q)
	}
	var req approvalRequest
	if err := readApprovalJSON(filepath.Join(approveDir(f.home, approvalSession), "requests", f.id+".json"), &req); err != nil {
		t.Fatal(err)
	}
	refused := json.RawMessage(`{"proof":1}`)
	adir, err := ensureApproveSubdir(f.home, approvalSession, "answers")
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceJSON(adir, f.id+".json", approvalAnswer{InteractionID: f.id, ActionHash: req.ActionHash, Option: optionAllow, Evidence: refused}); err != nil {
		t.Fatal(err)
	}
	if err := markRejected(f.home, approvalSession, f.id, refused, context.DeadlineExceeded.Error()); err != nil {
		t.Fatal(err)
	}
	allowed := make(chan protocol.Code, 1)
	go func() {
		code, _ := f.att.RespondWithEvidence(pr2Key(), "", f.id, optionAllow, json.RawMessage(`{"proof":2}`))
		allowed <- code
	}()
	<-entered
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionDeny); err != nil || code != "" {
		t.Fatalf("❌ = %q, %v; want it written", code, err)
	}
	close(release)
	code := <-allowed
	var onDisk approvalAnswer
	if err := readApprovalJSON(filepath.Join(adir, f.id+".json"), &onDisk); err != nil {
		t.Fatal(err)
	}
	if code != protocol.CodeAlreadyResolved || onDisk.Option != optionDeny {
		t.Fatalf("✅ = %q with %q on disk; want already_resolved and the ❌ on disk", code, onDisk.Option)
	}
	f.hookExited()
}

// 611.42.6 (Pro review of #986 r1): a refused record for a proof the hook
// was still verifying made the endpoint drop the allow and take a ❌, and
// the hook's verify of that genuine proof then succeeded and printed
// allow. The hook now records its allow verdict first and then requires
// no refusal of that proof, so the ❌ the endpoint took is what applies.
func TestApprovalHookNeverAllowsAProofRefusedDuringItsVerify(t *testing.T) {
	f := newApprovalFixture(t, bashUse("toolu_1", "go test ./..."))
	owner := strings.Repeat("ab", 32)
	entered, release := make(chan struct{}), make(chan struct{})
	f.allow = AllowConfig{
		Owner: owner,
		Share: func(string) (AllowShare, error) {
			return AllowShare{Owner: owner, Body: strings.Repeat("cd", 32), Channel: "dm-1", Target: "cc-1"}, nil
		},
		Verify: func(context.Context, json.RawMessage, AllowCheck) error {
			close(entered)
			<-release
			return nil // the genuine ✅ verifies
		},
	}
	ticks := make(chan time.Time)
	f.raiseWith("go test ./...", ticks)
	f.question()
	var req approvalRequest
	if err := readApprovalJSON(filepath.Join(approveDir(f.home, approvalSession), "requests", f.id+".json"), &req); err != nil || !req.Approvable {
		t.Fatalf("request = %+v (%v), want an approvable call", req, err)
	}
	dir, err := ensureApproveSubdir(f.home, approvalSession, "answers")
	if err != nil {
		t.Fatal(err)
	}
	proof := json.RawMessage(`{"proof":1}`)
	ans, _ := json.Marshal(approvalAnswer{InteractionID: f.id, ActionHash: req.ActionHash, Option: optionAllow, Evidence: proof})
	if err := os.WriteFile(filepath.Join(dir, f.id+".json"), ans, 0o600); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Now()
	<-entered
	if err := markRejected(f.home, approvalSession, f.id, proof, "forged"); err != nil {
		t.Fatal(err)
	}
	if code, err := f.att.Respond(pr2Key(), "", f.id, optionDeny); err != nil || code != "" {
		t.Fatalf("❌ = %q, %v; want it written over the refused allow", code, err)
	}
	close(release)
	select {
	case ticks <- time.Now():
	case <-f.exited:
	}
	f.hookExited()
	if got := f.out.String(); strings.Contains(got, `"behavior":"allow"`) || !strings.Contains(got, `"behavior":"deny"`) {
		t.Fatalf("hook printed %q, want the ❌ the endpoint took as deny", got)
	}
	// Advisor review of #1000: the endpoint read the allow verdict beside
	// the refusal as allowed, so the DM said nothing about the refusal.
	if v, ok := hookVerdict(f.home, approvalSession, f.id, proof); !ok || v.Verdict != verdictRefused {
		t.Fatalf("verdict = %+v, %v; want the proof refused", v, ok)
	}
}
