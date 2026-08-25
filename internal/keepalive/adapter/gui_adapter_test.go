package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDefaultRegistryRegistersClaudeDesktopBehindCapabilityGate(t *testing.T) {
	registry := DefaultRegistry()
	// claude-desktop is now registered (it is an honest prefill-only seat),
	// but the capability gate in app.registerWithOptions refuses it under the
	// default zero-value minimum and under a submitted/unattended minimum.
	// The gate-behavior assertions live in internal/keepalive/app.
	if _, err := registry.Get("claude-desktop"); err != nil {
		t.Fatalf("DefaultRegistry().Get(%q) failed; claude-desktop should be registered: %v", "claude-desktop", err)
	}
	cap := (ClaudeDesktop{}).Capability()
	if cap.Delivery != DeliveryPrefilled {
		t.Fatalf("claude-desktop Delivery = %v, want prefilled", cap.Delivery)
	}
	if cap.Session != SessionNew {
		t.Fatalf("claude-desktop Session = %v, want new", cap.Session)
	}
	if !cap.RequiresHuman {
		t.Fatalf("claude-desktop RequiresHuman = false, want true")
	}
	// Default zero-value minimum must refuse the requires-human seat.
	if cap.Satisfies(Capability{}) {
		t.Fatal("claude-desktop satisfies the zero-value minimum; the default gate must refuse it")
	}
	// A submitted/unattended caller must also be refused.
	if cap.Satisfies(Capability{Delivery: DeliverySubmitted, RequiresHuman: false}) {
		t.Fatal("claude-desktop satisfies a submitted/unattended minimum; the gate must refuse it")
	}
	// Only an explicit human-handoff prefill caller is accepted.
	if !cap.Satisfies(Capability{Delivery: DeliveryPrefilled, Session: SessionNew, RequiresHuman: true}) {
		t.Fatal("claude-desktop does not satisfy the explicit prefill+new+requires-human minimum")
	}
}

func TestDefaultRegistryRegistersCodexAppBehindCapabilityGate(t *testing.T) {
	registry := DefaultRegistry()
	// codex-app is now registered (deep-link prefill seat); the capability gate
	// in app.registerWithOptions refuses it under the default zero-value minimum.
	if _, err := registry.Get("codex-app"); err != nil {
		t.Fatalf("DefaultRegistry().Get(%q) failed; codex-app should be registered: %v", "codex-app", err)
	}
	newCap, _ := (CodexApp{}).CapabilityForTarget(codexAppTargetNew)
	if newCap.Delivery != DeliveryPrefilled || newCap.Session != SessionNew || !newCap.RequiresHuman {
		t.Fatalf("codex-app:new capability = %+v, want prefilled+new+requires_human", newCap)
	}
	threadCap, _ := (CodexApp{}).CapabilityForTarget(codexAppTargetThreadPrefix + "01a01f5f-69d6-7dd0-868f-9376f3d2c0a1")
	if threadCap.Session != SessionExistingExact {
		t.Fatalf("codex-app:thread capability Session = %v, want existing-exact", threadCap.Session)
	}
	// Both targets are requires-human; default zero min (unattended) refuses them.
	if newCap.Satisfies(Capability{}) || threadCap.Satisfies(Capability{}) {
		t.Fatal("codex-app satisfies the zero-value minimum; the default gate must refuse it")
	}
	// A new-only target must NOT satisfy an existing-exact min (no downgrade).
	if newCap.Satisfies(Capability{Delivery: DeliveryPrefilled, Session: SessionExistingExact, RequiresHuman: true}) {
		t.Fatal("codex-app:new satisfies an existing-exact minimum; the gate must refuse the downgrade")
	}
	// The thread target DOES satisfy an existing-exact+prefill+RH-tolerant min.
	if !threadCap.Satisfies(Capability{Delivery: DeliveryPrefilled, Session: SessionExistingExact, RequiresHuman: true}) {
		t.Fatal("codex-app:thread does not satisfy the existing-exact+prefill+requires-human minimum")
	}
}

func TestCodexAppNormalizeTargetAcceptsNewAndThreadUUID(t *testing.T) {
	app := CodexApp{}
	if got, err := app.NormalizeTarget(" codex-app:new "); err != nil || got != codexAppTargetNew {
		t.Fatalf("NormalizeTarget(new) = %q, %v; want %q", got, err, codexAppTargetNew)
	}
	const uuid = "01a01f5f-69d6-7dd0-868f-9376f3d2c0a1"
	if got, err := app.NormalizeTarget("codex-app:thread:" + uuid); err != nil || got != codexAppTargetThreadPrefix+uuid {
		t.Fatalf("NormalizeTarget(thread) = %q, %v; want %q", got, err, codexAppTargetThreadPrefix+uuid)
	}
}

func TestCodexAppTargetRejectsUnsafeOrMalformedIdentity(t *testing.T) {
	for _, target := range []string{
		"",
		"ChatGPT",
		"codex-app:tab:window-1/tab-1", // legacy grammar, no longer supported
		"codex-app:thread:",            // empty uuid
		"codex-app:thread:01A01F5F-69D6-7DD0-868F-9376F3D2C0A1",       // uppercase
		"codex-app:thread:01a01f5f-69d6-7dd0-868f-9376f3d2c0a",        // 35 chars
		"codex-app:thread:../../../etc/passwd",                        // path traversal
		"codex-app:thread:01a01f5f-69d6-7dd0-868f-9376f3d2c0a1/extra", // extra segment
		"codex-app:existing",
	} {
		if _, err := (CodexApp{}).NormalizeTarget(target); err == nil {
			t.Fatalf("NormalizeTarget(%q) succeeded; want refusal", target)
		}
		if _, err := (CodexApp{}).CapabilityForTarget(target); err == nil {
			t.Fatalf("CapabilityForTarget(%q) succeeded; want refusal", target)
		}
	}
}

func TestClaudeDesktopNormalizeTargetAcceptsOnlyNewSession(t *testing.T) {
	app := ClaudeDesktop{}
	if got, err := app.NormalizeTarget(" claude-desktop:new "); err != nil || got != claudeDesktopTarget {
		t.Fatalf("NormalizeTarget() = %q, %v; want new-session target", got, err)
	}
	for _, target := range []string{"", "claude-desktop", "claude-desktop:existing", "claude-desktop:code:existing"} {
		if _, err := app.NormalizeTarget(target); err == nil {
			t.Fatalf("NormalizeTarget(%q) succeeded; want refusal", target)
		}
	}
}

func TestClaudeDesktopInjectPrefillsOnlyWithEscapedURL(t *testing.T) {
	skipNonDarwin(t)
	// The first runner call is the Inject-time identity Probe (the swift
	// scheme-handler probe); it must return the pinned bundle id. The second
	// call is the prefill open.
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: []byte(claudeDesktopBundleID + "\n")},
		{},
	}}
	payload := "hello world & q=x"
	if err := (ClaudeDesktop{Runner: runner}).Inject(context.Background(), claudeDesktopTarget, payload); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	// Prefill-only: exactly two runner calls (probe + open), no submit step.
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want probe + open (no submit)", len(runner.calls))
	}
	probeCall := runner.calls[0]
	if probeCall.name != "swift" {
		t.Fatalf("first call = %q, want swift scheme-handler probe", probeCall.name)
	}
	call := runner.calls[1]
	if call.name != "open" {
		t.Fatalf("second call = %q, want open", call.name)
	}
	if len(call.args) != 1 {
		t.Fatalf("open args = %#v, want a single URL argument", call.args)
	}
	// The payload must be query-escaped as the q parameter; &/space/= must be
	// percent- or plus-encoded, never concatenated raw into a script.
	wantURL := "claude://code/new?q=hello+world+%26+q%3Dx"
	if call.args[0] != wantURL {
		t.Fatalf("open URL = %q, want %q", call.args[0], wantURL)
	}
	if strings.Contains(call.args[0], payload) {
		t.Fatalf("raw payload leaked unescaped into URL: %q", call.args[0])
	}
	// No forbidden generic-automation APIs anywhere in the command line.
	for _, forbidden := range []string{"System Events", "keystroke", "the clipboard", "AXRaise", "activate"} {
		if strings.Contains(call.name, forbidden) || strings.Contains(strings.Join(call.args, " "), forbidden) {
			t.Fatalf("command line uses forbidden %q: %#v", forbidden, call)
		}
	}
}

func TestClaudeDesktopInjectFailsClosedOnIdentityMismatchWithoutOpen(t *testing.T) {
	skipNonDarwin(t)
	// The Inject write is scheme-bound, not identity-bound, so Inject must
	// revalidate identity and fail closed before emitting the open. A mismatched
	// bundle id on the Inject-time Probe must produce zero open calls.
	mismatch := &fakeCommandRunner{output: []byte("com.some.other.app\n")}
	injectErr := (ClaudeDesktop{Runner: mismatch}).Inject(context.Background(), claudeDesktopTarget, "payload")
	if injectErr == nil {
		t.Fatal("Inject() with mismatched bundle id succeeded; want fail-closed")
	}
	for _, c := range mismatch.calls {
		if c.name == "open" {
			t.Fatalf("Inject emitted an open call after identity mismatch; the write must be gated: %#v", c)
		}
	}
	if !strings.Contains(fmt.Sprint(injectErr), claudeDesktopBundleID) {
		t.Fatalf("Inject() error = %v, want it to surface the identity mismatch", injectErr)
	}
}

func TestClaudeDesktopProbeFailsClosedOnIdentityMismatch(t *testing.T) {
	skipNonDarwin(t)
	mismatch := &fakeCommandRunner{output: []byte("com.some.other.app\n")}
	err := (ClaudeDesktop{Runner: mismatch}).Probe(context.Background(), claudeDesktopTarget)
	if err == nil {
		t.Fatal("Probe() with mismatched bundle id succeeded; want fail-closed")
	}
	if !strings.Contains(fmt.Sprint(err), claudeDesktopBundleID) {
		t.Fatalf("Probe() error = %v, want it to name the expected bundle id", err)
	}
	// A command failure (e.g. app not installed) must also fail closed.
	failing := &fakeCommandRunner{output: []byte("no such app"), err: errors.New("exit status 1")}
	if err := (ClaudeDesktop{Runner: failing}).Probe(context.Background(), claudeDesktopTarget); err == nil {
		t.Fatal("Probe() with command failure succeeded; want fail-closed")
	}
}

func TestClaudeDesktopDiscoverReturnsSeatAfterIdentityProbe(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte(claudeDesktopBundleID + "\n")}
	target, err := (ClaudeDesktop{Runner: runner}).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if target != claudeDesktopTarget {
		t.Fatalf("Discover() target = %q, want %q", target, claudeDesktopTarget)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "swift" {
		t.Fatalf("calls = %#v, want one swift scheme-handler probe", runner.calls)
	}
}

func TestCodexAppInjectPrefillsOnlyWithEscapedURL(t *testing.T) {
	skipNonDarwin(t)
	// The first runner call is the Inject-time identity Probe (swift scheme
	// handler); it must return the pinned bundle id. The second call is the
	// prefill open. Tested for both targets.
	for _, tc := range []struct {
		name    string
		target  string
		wantURL string
	}{
		{name: "new", target: codexAppTargetNew, wantURL: "codex://threads/new?prompt=hello+world+%26+q%3Dx"},
		{name: "thread", target: codexAppTargetThreadPrefix + "01a01f5f-69d6-7dd0-868f-9376f3d2c0a1", wantURL: "codex://threads/01a01f5f-69d6-7dd0-868f-9376f3d2c0a1?prompt=hello+world+%26+q%3Dx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeCommandRunner{results: []fakeCommandResult{
				{output: []byte(codexAppBundleID + "\n")},
				{},
			}}
			payload := "hello world & q=x"
			if err := (CodexApp{Runner: runner}).Inject(context.Background(), tc.target, payload); err != nil {
				t.Fatalf("Inject() error = %v", err)
			}
			if len(runner.calls) != 2 {
				t.Fatalf("calls = %d, want probe + open (no submit)", len(runner.calls))
			}
			if runner.calls[0].name != "swift" {
				t.Fatalf("first call = %q, want swift scheme-handler probe", runner.calls[0].name)
			}
			openCall := runner.calls[1]
			if openCall.name != "open" {
				t.Fatalf("second call = %q, want open", openCall.name)
			}
			if len(openCall.args) != 1 || openCall.args[0] != tc.wantURL {
				t.Fatalf("open args = %#v, want single URL %q", openCall.args, tc.wantURL)
			}
			if strings.Contains(openCall.args[0], payload) {
				t.Fatalf("raw payload leaked unescaped into URL: %q", openCall.args[0])
			}
			// No forbidden generic-automation APIs anywhere in the command line.
			for _, forbidden := range []string{"System Events", "keystroke", "the clipboard", "AXRaise", "activate"} {
				if strings.Contains(openCall.name, forbidden) || strings.Contains(strings.Join(openCall.args, " "), forbidden) {
					t.Fatalf("command line uses forbidden %q: %#v", forbidden, openCall)
				}
			}
		})
	}
}

func TestCodexAppInjectFailsClosedOnIdentityMismatchWithoutOpen(t *testing.T) {
	skipNonDarwin(t)
	// The Inject write is scheme-bound, so Inject must revalidate identity and
	// fail closed before emitting the open. A mismatched bundle id on the
	// Inject-time Probe must produce zero open calls.
	mismatch := &fakeCommandRunner{output: []byte("com.some.other.app\n")}
	injectErr := (CodexApp{Runner: mismatch}).Inject(context.Background(), codexAppTargetNew, "payload")
	if injectErr == nil {
		t.Fatal("Inject() with mismatched bundle id succeeded; want fail-closed")
	}
	for _, c := range mismatch.calls {
		if c.name == "open" {
			t.Fatalf("Inject emitted an open call after identity mismatch; the write must be gated: %#v", c)
		}
	}
	if !strings.Contains(fmt.Sprint(injectErr), codexAppBundleID) {
		t.Fatalf("Inject() error = %v, want it to surface the identity mismatch", injectErr)
	}
}

func TestCodexAppProbeFailsClosedOnIdentityMismatch(t *testing.T) {
	skipNonDarwin(t)
	mismatch := &fakeCommandRunner{output: []byte("com.some.other.app\n")}
	if err := (CodexApp{Runner: mismatch}).Probe(context.Background(), codexAppTargetNew); err == nil {
		t.Fatal("Probe() with mismatched bundle id succeeded; want fail-closed")
	}
	failing := &fakeCommandRunner{output: []byte("no such app"), err: errors.New("exit status 1")}
	if err := (CodexApp{Runner: failing}).Probe(context.Background(), codexAppTargetNew); err == nil {
		t.Fatal("Probe() with command failure succeeded; want fail-closed")
	}
}

func TestCodexAppDiscoverReturnsNewSeatAfterIdentityProbe(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte(codexAppBundleID + "\n")}
	target, err := (CodexApp{Runner: runner}).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if target != codexAppTargetNew {
		t.Fatalf("Discover() target = %q, want %q", target, codexAppTargetNew)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "swift" {
		t.Fatalf("calls = %#v, want one swift scheme-handler probe", runner.calls)
	}
}
