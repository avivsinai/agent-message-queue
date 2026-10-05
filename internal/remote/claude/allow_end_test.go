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
