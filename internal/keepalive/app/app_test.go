package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

type appCommandRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f appCommandRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestHelpWritesUsageToStdoutAndExitsZero(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"--help"})
	if code != 0 {
		t.Fatalf("help code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "usage: amq-keepalive") {
		t.Fatalf("stdout does not contain usage: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "amq-keepalive <-v|--version>") ||
		!strings.Contains(stdout.String(), "|version>") {
		t.Fatalf("stdout does not advertise version spellings: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestReattachReplacesCurrentSessionAdapterEntry(t *testing.T) {
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	oldTarget := filepath.Join(dir, "old-inbox.txt")
	newTarget := filepath.Join(dir, "new-inbox.txt")
	otherTarget := filepath.Join(dir, "other-inbox.txt")
	canonicalNewTarget := normalizedFileTarget(t, newTarget)
	canonicalOtherTarget := normalizedFileTarget(t, otherTarget)

	runApp(t, "attach",
		"--registry", registryPath,
		"--adapter", "file",
		"--target", oldTarget,
		"--root", "/tmp/amq-root",
		"--base-root", "/tmp",
		"--session", "amq-root",
		"--me", "codex",
		"--no-start",
	)
	runApp(t, "attach",
		"--registry", registryPath,
		"--adapter", "file",
		"--target", otherTarget,
		"--root", "/tmp/amq-root",
		"--base-root", "/tmp",
		"--session", "amq-root",
		"--me", "claude",
		"--no-start",
	)

	runApp(t, "reattach",
		"--registry", registryPath,
		"--adapter", "file",
		"--target", newTarget,
		"--root", "/tmp/amq-root",
		"--base-root", "/tmp",
		"--session", "amq-root",
		"--me", "codex",
		"--no-start",
	)

	loaded, err := registry.New(registryPath).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %#v", len(loaded.Entries), loaded.Entries)
	}
	targets := map[string]string{}
	for _, entry := range loaded.Entries {
		targets[entry.Agent] = entry.Target
	}
	if targets["codex"] != canonicalNewTarget {
		t.Fatalf("codex target = %q, want normalized %q", targets["codex"], canonicalNewTarget)
	}
	if targets["claude"] != canonicalOtherTarget {
		t.Fatalf("claude target = %q, want normalized %q", targets["claude"], canonicalOtherTarget)
	}
}

func TestAttachRejectsCanonicalRootAliasWithoutStartingWake(t *testing.T) {
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	realRoot := filepath.Join(dir, "real-root")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatalf("Mkdir root: %v", err)
	}
	aliasRoot := filepath.Join(dir, "alias-root")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatalf("Symlink root alias: %v", err)
	}
	canonicalRoot, err := registry.CanonicalRoot(realRoot)
	if err != nil {
		t.Fatalf("CanonicalRoot(real root): %v", err)
	}
	firstTarget := filepath.Join(dir, "first-inbox.txt")
	secondTarget := filepath.Join(dir, "second-inbox.txt")
	canonicalFirstTarget, err := (adapter.File{}).NormalizeTarget(firstTarget)
	if err != nil {
		t.Fatalf("NormalizeTarget(first target): %v", err)
	}
	runApp(t, "attach", "--registry", registryPath, "--adapter", "file", "--target", firstTarget,
		"--root", realRoot, "--base-root", dir, "--session", "real-root", "--me", "codex", "--no-start")

	amqCalls := filepath.Join(dir, "amq-calls.log")
	fakeAMQ := filepath.Join(dir, "amq")
	if err := os.WriteFile(fakeAMQ, []byte("#!/bin/sh\nprintf wake >> \"$AMQ_KEEPALIVE_AMQ_CALLS\"\nexit 7\n"), 0o700); err != nil {
		t.Fatalf("write fake amq: %v", err)
	}
	t.Setenv("AMQ_KEEPALIVE_AMQ_CALLS", amqCalls)
	var stderr bytes.Buffer
	code := (App{Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{
		"attach", "--registry", registryPath, "--adapter", "file", "--target", secondTarget,
		"--root", aliasRoot, "--base-root", dir, "--session", "real-root", "--me", "codex", "--amq", fakeAMQ,
	})
	if code != 1 || !strings.Contains(stderr.String(), registry.ErrRegistrationOwned.Error()) {
		t.Fatalf("alias attach code=%d stderr=%s, want root-agent ownership refusal", code, stderr.String())
	}
	if _, err := os.Stat(amqCalls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected second attach started a wake: stat err=%v", err)
	}
	loaded, err := registry.New(registryPath).Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0].Root != canonicalRoot || loaded.Entries[0].Target != canonicalFirstTarget {
		t.Fatalf("alias attach poisoned registry: entries=%#v err=%v", loaded.Entries, err)
	}
}

func TestAttachIsIdempotentForSamePhysicalCmuxOwner(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	target := "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"
	runner := appCommandRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"windows":[{"workspaces":[{"id":"WS-1","panes":[{"surfaces":[{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"ttys101"}]}]}]}]}`), nil
	})
	adapters := adapter.NewRegistry(adapter.Cmux{
		Runner: runner,
		Path:   "/fake/cmux",
		LiveTTYOwnerCount: func(string) (int, error) {
			return 1, nil
		},
	}.WithOwnershipRecord())
	args := []string{
		"attach", "--registry", registryPath, "--adapter", "cmux", "--target", target,
		"--root", "/tmp/idempotent", "--base-root", "/tmp", "--session", "idempotent", "--me", "codex", "--no-start",
	}
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		code := (App{Stdout: &stdout, Stderr: &stderr, Adapters: &adapters}).Run(context.Background(), args)
		if code != 0 {
			t.Fatalf("attach pass %d code=%d stdout=%s stderr=%s", i+1, code, stdout.String(), stderr.String())
		}
	}
	loaded, err := registry.New(registryPath).Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0].Target != target {
		t.Fatalf("idempotent attach entries=%#v err=%v", loaded.Entries, err)
	}
	if loaded.Entries[0].OwnershipKey != "tty:/dev/ttys101" {
		t.Fatalf("ownership_key = %q, want persisted tty key", loaded.Entries[0].OwnershipKey)
	}
}

func TestReattachRejectsDifferentSurfaceAliasOnOwnedPhysicalTTY(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	firstTarget := "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"
	secondTarget := "cmux:surface:B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	store := registry.New(registryPath)
	if _, err := store.Upsert(registry.Entry{Root: "/tmp/first", Agent: "codex", Adapter: "cmux", Target: firstTarget}); err != nil {
		t.Fatalf("Upsert first owner: %v", err)
	}
	runner := appCommandRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"windows":[{"workspaces":[{"id":"WS-1","panes":[{"surfaces":[{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"/dev/ttys011"},{"id":"B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2","tty":"ttys011"}]}]}]}]}`), nil
	})
	adapters := adapter.NewRegistry(adapter.Cmux{
		Runner: runner,
		Path:   "/fake/cmux",
		Getenv: func(key string) string {
			if key == "CMUX_SURFACE_ID" {
				return strings.TrimPrefix(firstTarget, "cmux:surface:")
			}
			return ""
		},
		LiveTTYOwnerCount: func(string) (int, error) {
			return 1, nil
		},
	})
	var stderr bytes.Buffer
	code := (App{Stdout: &bytes.Buffer{}, Stderr: &stderr, Adapters: &adapters}).Run(context.Background(), []string{
		"reattach", "--registry", registryPath, "--adapter", "cmux", "--target", secondTarget,
		"--root", "/tmp/second", "--base-root", "/tmp", "--session", "second", "--me", "claude", "--no-start",
	})
	if code != 1 || !strings.Contains(stderr.String(), "2 live surface aliases") {
		t.Fatalf("code=%d stderr=%s, want physical ownership refusal", code, stderr.String())
	}
	loaded, err := store.Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0].Target != firstTarget {
		t.Fatalf("physical collision changed registry: entries=%#v err=%v", loaded.Entries, err)
	}
}

func TestConcurrentReattachClaimStartsOnlyWinningWake(t *testing.T) {
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	target := filepath.Join(dir, "inbox.txt")
	canonicalTarget := normalizedFileTarget(t, target)
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	calls := filepath.Join(dir, "amq-calls.log")
	t.Setenv("AMQ_KEEPALIVE_AMQ_CALLS", calls)
	fakeAMQ := filepath.Join(dir, "amq")
	if err := os.WriteFile(fakeAMQ, fakeStartWakeScript(`#!/bin/sh
printf 'wake\n' >> "$AMQ_KEEPALIVE_AMQ_CALLS"
ready=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "-ready-file" ]; then ready="$arg"; fi
  previous="$arg"
done
[ -n "$ready" ] || exit 11
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
sleep 0.1
`), 0o700); err != nil {
		t.Fatalf("write fake amq: %v", err)
	}
	start := make(chan struct{})
	codes := make(chan int, 2)
	for index := 0; index < 2; index++ {
		index := index
		go func() {
			<-start
			codes <- (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).Run(context.Background(), []string{
				"reattach", "--registry", registryPath, "--adapter", "file", "--target", target,
				"--root", fmt.Sprintf("/tmp/race-%d", index), "--base-root", "/tmp",
				"--session", fmt.Sprintf("race-%d", index), "--me", fmt.Sprintf("agent-%d", index),
				"--amq", fakeAMQ, "--self", "/bin/amq-keepalive",
			})
		}()
	}
	close(start)
	firstCode, secondCode := <-codes, <-codes
	if firstCode+secondCode != 1 {
		t.Fatalf("concurrent codes=(%d,%d), want one success and one ownership refusal", firstCode, secondCode)
	}
	data, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(data), "wake\n") != 1 {
		t.Fatalf("wake calls=%q err=%v, want exactly one start", data, err)
	}
	loaded, err := registry.New(registryPath).Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0].Target != canonicalTarget {
		t.Fatalf("winning registry entries=%#v err=%v", loaded.Entries, err)
	}
}

func TestRegisterCapabilityGateRefusesClaudeDesktopUnderWeakMinimum(t *testing.T) {
	// The capability gate must refuse a requires-human prefill seat under the
	// default zero-value minimum (today's implicit unattended minimum) and
	// under a submitted/unattended minimum. No substitution, no fallback.
	base := registerOptions{
		AdapterName:  "claude-desktop",
		Target:       "claude-desktop:new",
		Root:         "/tmp/amq-root",
		BaseRoot:     "/tmp",
		SessionName:  "amq-root",
		Me:           "codex",
		NoStart:      true,
		RegistryPath: testRegistryTempPath(t),
	}
	for _, tc := range []struct {
		name string
		min  adapter.Capability
	}{
		{name: "zero-value default minimum", min: adapter.Capability{}},
		{name: "submitted unattended minimum", min: adapter.Capability{Delivery: adapter.DeliverySubmitted, RequiresHuman: false}},
		{name: "existing-exact minimum", min: adapter.Capability{Session: adapter.SessionExistingExact}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callCount := 0
			trackingRunner := appCommandRunnerFunc(func(_ context.Context, name string, _ ...string) ([]byte, error) {
				callCount++
				return []byte("com.anthropic.claudefordesktop\n"), nil
			})
			trackingAdapters := adapter.NewRegistry(adapter.ClaudeDesktop{Runner: trackingRunner})
			registryPath := testRegistryTempPath(t)
			opts := base
			opts.RegistryPath = registryPath
			opts.MinCapability = tc.min
			app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Adapters: &trackingAdapters}
			err := app.registerWithOptions(context.Background(), opts)
			if err == nil {
				t.Fatalf("registerWithOptions succeeded under %s; want capability refusal", tc.name)
			}
			if !strings.Contains(err.Error(), "does not satisfy") {
				t.Fatalf("error = %v, want a capability refusal naming the shortfall", err)
			}
			// FIX-5: a refusal must happen before discovery/injection, so the
			// runner is never invoked and the registry file is left untouched.
			if callCount != 0 {
				t.Fatalf("runner called %d time(s) on refusal; want 0 (gate fires before any command)", callCount)
			}
			if info, statErr := os.Stat(registryPath); !os.IsNotExist(statErr) {
				t.Fatalf("registry file exists after refusal (stat=%v %v); want it never written", info, statErr)
			}
		})
	}
}

// undeclaredAdapter implements adapter.Adapter but NOT CapabilityDeclarer, so
// it is treated as UnknownCapability() (weakest on every ordered axis and
// requires a human). This is the FIX-A regression: such an adapter must NOT
// bypass the gate — it is refused under the default zero-value minimum (which
// is unattended) and under a submitted minimum, and accepted only when the
// caller explicitly tolerates a human-required seat.
type undeclaredAdapter struct{}

func (undeclaredAdapter) Name() string                                 { return "undeclared" }
func (undeclaredAdapter) Probe(context.Context, string) error          { return nil }
func (undeclaredAdapter) Inject(context.Context, string, string) error { return nil }

type injectProgressAdapter struct {
	name string
}

func (a injectProgressAdapter) Name() string                               { return a.name }
func (injectProgressAdapter) Probe(context.Context, string) error          { return nil }
func (injectProgressAdapter) Inject(context.Context, string, string) error { return nil }

type uncertainInjectProgressAdapter struct {
	injectProgressAdapter
}

func (uncertainInjectProgressAdapter) Inject(context.Context, string, string) error {
	return fmt.Errorf("submit failed: %w: %s", adapter.ErrInjectUncertain, adapter.ErrInjectUncertain)
}

type acceptedInjectProgressAdapter struct {
	injectProgressAdapter
}

func (acceptedInjectProgressAdapter) ReportsProviderAcceptance() {}

func TestInjectReportsProviderAcceptanceOnlyForOptInAdapters(t *testing.T) {
	tests := []struct {
		name       string
		adapter    adapter.Adapter
		wantMarker bool
	}{
		{
			name:    "transport-only adapter",
			adapter: injectProgressAdapter{name: "transport-only"},
		},
		{
			name: "provider-ack adapter",
			adapter: acceptedInjectProgressAdapter{
				injectProgressAdapter: injectProgressAdapter{name: "provider-ack"},
			},
			wantMarker: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapters := adapter.NewRegistry(test.adapter)
			var stderr bytes.Buffer
			code := (App{Stdout: &bytes.Buffer{}, Stderr: &stderr, Adapters: &adapters}).Run(
				context.Background(),
				[]string{"inject", test.adapter.Name(), "target", "payload"},
			)
			if code != 0 {
				t.Fatalf("inject code = %d, stderr = %q", code, stderr.String())
			}
			gotMarker := strings.Contains(stderr.String(), "AMQ_INJECT_PROGRESS=accepted")
			if gotMarker != test.wantMarker {
				t.Fatalf("accepted marker = %t, want %t; stderr = %q", gotMarker, test.wantMarker, stderr.String())
			}
		})
	}
}

func TestRegisterCapabilityGateTreatsUndeclaredAdapterAsUnknown(t *testing.T) {
	// An undeclared adapter is treated as UnknownCapability(): weakest on
	// every ordered axis and requires a human. It is REFUSED under the default
	// zero-value minimum (which is unattended) AND under a submitted minimum,
	// and ACCEPTED only when the caller explicitly tolerates a human-required
	// seat. This is the FIX-A regression: an unknown adapter must never
	// masquerade as unattended full-strength.
	adapters := adapter.NewRegistry(undeclaredAdapter{})
	base := registerOptions{
		AdapterName:  "undeclared",
		Target:       "claude-desktop:new",
		Root:         "/tmp/amq-root",
		BaseRoot:     "/tmp",
		SessionName:  "amq-root",
		Me:           "codex",
		NoStart:      true,
		RegistryPath: testRegistryTempPath(t),
	}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Adapters: &adapters}
	for _, tc := range []struct {
		name string
		min  adapter.Capability
	}{
		{name: "zero-value default minimum", min: adapter.Capability{}},
		{name: "submitted unattended minimum", min: adapter.Capability{Delivery: adapter.DeliverySubmitted, RequiresHuman: false}},
	} {
		t.Run(tc.name+" refuses", func(t *testing.T) {
			opts := base
			opts.RegistryPath = testRegistryTempPath(t)
			opts.MinCapability = tc.min
			if err := app.registerWithOptions(context.Background(), opts); err == nil {
				t.Fatalf("undeclared adapter accepted under %s; want refusal", tc.name)
			} else if !strings.Contains(err.Error(), "does not satisfy") {
				t.Fatalf("error = %v, want a capability refusal", err)
			}
		})
	}
	// Accepted only when the caller explicitly tolerates a human-required seat.
	acceptOpts := base
	acceptOpts.RegistryPath = testRegistryTempPath(t)
	acceptOpts.MinCapability = adapter.Capability{RequiresHuman: true}
	if err := app.registerWithOptions(context.Background(), acceptOpts); err != nil {
		t.Fatalf("undeclared adapter under explicit requires-human min failed: %v", err)
	}
}

func TestRegisterCapabilityGateAcceptsClaudePrintSubmittedSeat(t *testing.T) {
	const uuid = "a616af69-92db-495e-9691-c512c80c4bd6"
	cwd := t.TempDir()
	configDir := t.TempDir()
	proj := filepath.Join(configDir, "projects", "-tmp-scratch")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	rec, err := json.Marshal(map[string]string{"type": "user", "cwd": cwd, "sessionId": uuid})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, uuid+".jsonl"), append(rec, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configDir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := appCommandRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("Usage: --resume --output-format --replay-user-messages\n"), nil
	})
	seat := adapter.ClaudePrint{
		Runner:    runner,
		LookPath:  func(string) (string, error) { return bin, nil },
		ConfigDir: configDir,
		StateDir:  t.TempDir(),
	}
	adapters := adapter.NewRegistry(seat)
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Adapters: &adapters}
	acceptOpts := registerOptions{
		AdapterName:  "claude-print",
		Target:       "claude-print:session:" + uuid,
		Root:         "/tmp/amq-root",
		BaseRoot:     "/tmp",
		SessionName:  "amq-root",
		Me:           "claude",
		AMQPath:      "/bin/false",
		NoStart:      true,
		RegistryPath: testRegistryTempPath(t),
		MinCapability: adapter.Capability{
			Delivery: adapter.DeliverySubmitted,
			Session:  adapter.SessionExistingExact,
		},
	}
	if err := app.registerWithOptions(context.Background(), acceptOpts); err != nil {
		t.Fatalf("claude-print under submitted+existing-exact min failed: %v", err)
	}
	refuseOpts := acceptOpts
	refuseOpts.RegistryPath = testRegistryTempPath(t)
	refuseOpts.MinCapability = adapter.Capability{
		Activation: adapter.ActivationForeground,
		Delivery:   adapter.DeliverySubmitted,
		Session:    adapter.SessionExistingExact,
	}
	if err := app.registerWithOptions(context.Background(), refuseOpts); err == nil {
		t.Fatal("claude-print under foreground min succeeded; want refusal (activation none)")
	} else if !strings.Contains(err.Error(), "does not satisfy") {
		t.Fatalf("error = %v, want a capability refusal", err)
	}
}

func TestReattachPersistsRecoverableReservationBeforeWakeReadiness(t *testing.T) {
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	oldTarget := filepath.Join(dir, "old-inbox.txt")
	newTarget := filepath.Join(dir, "new-inbox.txt")
	canonicalOldTarget := normalizedFileTarget(t, oldTarget)
	canonicalNewTarget := normalizedFileTarget(t, newTarget)
	runApp(t, "attach",
		"--registry", registryPath,
		"--adapter", "file",
		"--target", oldTarget,
		"--root", "/tmp/amq-root",
		"--base-root", "/tmp",
		"--session", "amq-root",
		"--me", "codex",
		"--no-start",
	)
	wake := &appBlockingWake{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("amq wake exited before becoming ready (deterministic fake)"),
	}
	t.Cleanup(wake.Release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- (App{
			Stdout: &bytes.Buffer{},
			Stderr: &bytes.Buffer{},
			Wake:   wake,
		}).Run(ctx, []string{
			"reattach",
			"--registry", registryPath,
			"--adapter", "file",
			"--target", newTarget,
			"--root", "/tmp/amq-root",
			"--base-root", "/tmp",
			"--session", "amq-root",
			"--me", "codex",
			"--wake-ready-timeout", "5s",
		})
	}()
	select {
	case <-wake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for injected wake start")
	}

	loaded, err := registry.New(registryPath).Load()
	if err != nil {
		t.Fatalf("Load(in-flight) error = %v", err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Target != canonicalNewTarget || loaded.Entries[0].State != registry.StateAttached {
		t.Fatalf("in-flight entries = %#v, want inactive candidate reservation", loaded.Entries)
	}
	wake.Release()
	if code := <-done; code != 1 {
		t.Fatalf("reattach code = %d, want failure", code)
	}
	loaded, err = registry.New(registryPath).Load()
	if err != nil {
		t.Fatalf("Load(final) error = %v", err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Target != canonicalOldTarget {
		t.Fatalf("final entries = %#v, want old target preserved", loaded.Entries)
	}
}

func TestReattachRetireDetachedRetiresPreviousWakeThenStarts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "probe-room")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll root: %v", err)
	}
	registryPath := testRegistryPath(t, dir)
	oldTarget := "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"
	newTarget := "cmux:surface:B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	store := registry.New(registryPath)
	if _, err := store.Upsert(registry.Entry{
		Root: root, BaseRoot: dir, SessionName: "probe-room", Agent: "codex",
		Adapter: "cmux", Target: oldTarget, State: registry.StateDetached,
	}); err != nil {
		t.Fatalf("Upsert old entry: %v", err)
	}
	fakeCmux := filepath.Join(dir, "cmux")
	if err := os.WriteFile(fakeCmux, []byte("#!/bin/sh\necho '{\"windows\":[{\"workspaces\":[{\"panes\":[{\"surfaces\":[{\"id\":\"B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2\",\"tty\":\"ttys102\"}]}]}]}]}'\n"), 0o700); err != nil {
		t.Fatalf("write fake cmux: %v", err)
	}
	t.Setenv("CMUX_BUNDLED_CLI_PATH", fakeCmux)
	argsLog := filepath.Join(dir, "amq-args.log")
	firstStart := filepath.Join(dir, "first-start")
	t.Setenv("AMQ_KEEPALIVE_ARGS_LOG", argsLog)
	t.Setenv("AMQ_KEEPALIVE_FIRST_START", firstStart)
	fakeAMQ := filepath.Join(dir, "amq")
	if err := os.WriteFile(fakeAMQ, fakeStartWakeScript(`#!/bin/sh
printf 'CALL %s\n' "$*" >> "$AMQ_KEEPALIVE_ARGS_LOG"
if [ "$1" = "wake" ] && [ "${2:-}" = "retire" ]; then
  echo '{"status":"retired","agent":"codex","pid":4242}'
  exit 0
fi
if [ ! -f "$AMQ_KEEPALIVE_FIRST_START" ]; then
  : > "$AMQ_KEEPALIVE_FIRST_START"
  echo 'existing wake target differs' >&2
  exit 7
fi
previous=""
for arg in "$@"; do
  if [ "$previous" = "-ready-file" ]; then umask 077; printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$arg"; fi
  previous="$arg"
done
`), 0o700); err != nil {
		t.Fatalf("write fake AMQ: %v", err)
	}
	fakeKeepalive := filepath.Join(dir, "amq-keepalive")
	if err := os.WriteFile(fakeKeepalive, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake keepalive: %v", err)
	}
	adapters := hermeticCmuxRegistry(fakeCmux)
	var stderr bytes.Buffer
	code := (App{Stdout: &bytes.Buffer{}, Stderr: &stderr, Adapters: &adapters}).Run(context.Background(), []string{"reattach",
		"--registry", registryPath,
		"--adapter", "cmux",
		"--target", newTarget,
		"--root", root,
		"--base-root", dir,
		"--session", "probe-room",
		"--me", "codex",
		"--amq", fakeAMQ,
		"--self", fakeKeepalive,
		"--retire-detached",
	})
	if code != 0 {
		t.Fatalf("code=%d stderr=%s, want retire-then-start success", code, stderr.String())
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Target != newTarget || loaded.Entries[0].State != registry.StateActive {
		t.Fatalf("entries = %#v, want new target active after retiring the previous wake", loaded.Entries)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	log := string(data)
	if starts := strings.Count(log, "CALL wake -root"); starts != 2 {
		t.Fatalf("wake starts = %d, want one failed start then one post-retire start:\n%s", starts, log)
	}
	if !strings.Contains(log, "wake retire") || !strings.Contains(log, oldTarget) {
		t.Fatalf("retire path must retire the previous (old) target before restarting:\n%s", log)
	}
}

func TestRetireSessionRetiresConfirmedWakesAndRemovesRows(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "dashboard")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll root: %v", err)
	}
	registryPath := testRegistryPath(t, dir)
	store := registry.New(registryPath)
	entries := []registry.Entry{
		{Root: root, BaseRoot: dir, SessionName: "dashboard", Agent: "codex", Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3", State: registry.StateDetached},
		{Root: root, BaseRoot: dir, SessionName: "dashboard", Agent: "claude", Adapter: "cmux", Target: "cmux:surface:B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2", State: registry.StateDetached},
		{Root: root, BaseRoot: dir, SessionName: "dashboard", Agent: "observer", Adapter: "file", Target: filepath.Join(dir, "observer.txt"), State: registry.StateActive},
	}
	for _, entry := range entries {
		if _, err := store.Upsert(entry); err != nil {
			t.Fatalf("Upsert(%s): %v", entry.Agent, err)
		}
	}

	fakeCmux := filepath.Join(dir, "cmux")
	if err := os.WriteFile(fakeCmux, []byte("#!/bin/sh\necho '{\"windows\":[]}'\n"), 0o700); err != nil {
		t.Fatalf("write fake cmux: %v", err)
	}
	t.Setenv("CMUX_BUNDLED_CLI_PATH", fakeCmux)
	argsLog := filepath.Join(dir, "amq-args.log")
	t.Setenv("AMQ_KEEPALIVE_ARGS_LOG", argsLog)
	fakeAMQ := filepath.Join(dir, "amq")
	if err := os.WriteFile(fakeAMQ, fakeAMQWakeCheckThen(`
printf 'RETIRE %s\n' "$*" >> "$AMQ_KEEPALIVE_ARGS_LOG"
printf '{"status":"retired","pid":4242}\n'
`), 0o700); err != nil {
		t.Fatalf("write fake AMQ: %v", err)
	}
	fakeKeepalive := filepath.Join(dir, "amq-keepalive")
	if err := os.WriteFile(fakeKeepalive, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake keepalive: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{
		"retire-session",
		"--registry", registryPath,
		"--root", root,
		"--agents", "codex,claude",
		"--adapter", "cmux",
		"--amq", fakeAMQ,
		"--self", fakeKeepalive,
	})
	if code != 0 {
		t.Fatalf("retire-session code=%d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Agent != "observer" {
		t.Fatalf("entries after retirement = %#v, want only the non-cmux observer row preserved", loaded.Entries)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read AMQ log: %v", err)
	}
	if retires := strings.Count(string(data), "wake retire"); retires != 2 {
		t.Fatalf("wake retire calls = %d, want one per confirmed cmux agent:\n%s", retires, data)
	}
	for _, agent := range []string{"codex", "claude"} {
		if !strings.Contains(string(data), "--me "+agent) {
			t.Fatalf("retire log missing agent %q:\n%s", agent, data)
		}
	}
}

func TestRetireSessionRefusesWhenTargetStillExists(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "dashboard")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll root: %v", err)
	}
	registryPath := testRegistryPath(t, dir)
	store := registry.New(registryPath)
	for _, entry := range []registry.Entry{
		{Root: root, Agent: "codex", Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3", State: registry.StateDetached},
		{Root: root, Agent: "claude", Adapter: "cmux", Target: "cmux:surface:B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2", State: registry.StateDetached},
	} {
		if _, err := store.Upsert(entry); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	fakeCmux := filepath.Join(dir, "cmux")
	if err := os.WriteFile(fakeCmux, []byte("#!/bin/sh\necho '{\"windows\":[{\"workspaces\":[{\"panes\":[{\"surfaces\":[{\"id\":\"F901D722-6789-4BBB-9818-C4E97F20BEB3\",\"tty\":\"ttys101\"},{\"id\":\"B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2\",\"tty\":\"ttys102\"}]}]}]}]}'\n"), 0o700); err != nil {
		t.Fatalf("write fake cmux: %v", err)
	}
	t.Setenv("CMUX_BUNDLED_CLI_PATH", fakeCmux)
	fakeAMQ := filepath.Join(dir, "amq")
	if err := os.WriteFile(fakeAMQ, fakeAMQWakeCheckThen("exit 99\n"), 0o700); err != nil {
		t.Fatalf("write fake AMQ: %v", err)
	}
	adapters := hermeticCmuxRegistry(fakeCmux)
	var stderr bytes.Buffer
	code := (App{Stdout: &bytes.Buffer{}, Stderr: &stderr, Adapters: &adapters}).Run(context.Background(), []string{
		"retire-session", "--registry", registryPath, "--root", root, "--amq", fakeAMQ,
	})
	if code != 1 || !strings.Contains(stderr.String(), "still exists") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	loaded, err := store.Load()
	if err != nil || len(loaded.Entries) != 2 {
		t.Fatalf("registry changed after refusal: entries=%#v err=%v", loaded.Entries, err)
	}
}

func TestInstallHookCommandWritesRequestedConfig(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "amq-keepalive")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	runApp(t, "install-hook",
		"--agent", "codex",
		"--script", filepath.Join(dir, "hook.sh"),
		"--bin", binaryPath,
		"--codex-config", filepath.Join(dir, "hooks.json"),
		"--timeout", "1s",
	)
	if _, err := os.Stat(filepath.Join(dir, "hook.sh")); err != nil {
		t.Fatalf("hook not installed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks config: %v", err)
	}
	if !bytes.Contains(data, []byte("AMQ_KEEPALIVE_TIMEOUT_SECONDS='1'")) {
		t.Fatalf("hooks config missing timeout command:\n%s", data)
	}
}

type appBlockingWake struct {
	started     chan struct{}
	release     chan struct{}
	err         error
	startOnce   sync.Once
	releaseOnce sync.Once
}

func (w *appBlockingWake) StartWake(ctx context.Context, _ amq.StartWakeRequest) error {
	w.startOnce.Do(func() { close(w.started) })
	select {
	case <-w.release:
		return w.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *appBlockingWake) Release() {
	w.releaseOnce.Do(func() { close(w.release) })
}

type appCountingWake struct {
	starts []amq.StartWakeRequest
}

func (w *appCountingWake) StartWake(_ context.Context, req amq.StartWakeRequest) error {
	w.starts = append(w.starts, req)
	return nil
}

type appCancelingWake struct {
	cancel context.CancelFunc
	starts int
}

func (w *appCancelingWake) StartWake(ctx context.Context, _ amq.StartWakeRequest) error {
	w.starts++
	w.cancel()
	return ctx.Err()
}

func TestSuperviseCancellationStopsLaterStartsAndLeavesRegistryUnchanged(t *testing.T) {
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	store := registry.New(registryPath)
	for index := 0; index < 2; index++ {
		target := filepath.Join(dir, fmt.Sprintf("target-%d", index))
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if _, err := store.Upsert(registry.Entry{
			Root: fmt.Sprintf("/tmp/cancel-%d", index), Agent: "codex", Adapter: "file", Target: target,
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	before, err := store.Load()
	if err != nil {
		t.Fatalf("Load before: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wake := &appCancelingWake{cancel: cancel}
	results, err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).superviseOnce(
		ctx, registryPath, wake, "/bin/amq-keepalive", time.Second,
	)
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if wake.starts != 1 {
		t.Fatalf("wake starts=%d, want first only", wake.starts)
	}
	after, err := store.Load()
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("registry mutated on cancellation: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestSuperviseBatchesCmuxInventoryAndDefersHealthyEntries(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	store := registry.New(registryPath)
	targets := []string{
		"cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3",
		"cmux:surface:B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2",
	}
	for i, target := range targets {
		if _, err := store.Upsert(registry.Entry{Root: filepath.Join(dir, string(rune('a'+i))), Agent: "codex", Adapter: "cmux", Target: target}); err != nil {
			t.Fatalf("Upsert(%s): %v", target, err)
		}
	}
	calls := filepath.Join(dir, "cmux-calls.log")
	t.Setenv("AMQ_KEEPALIVE_CMUX_CALLS", calls)
	fakeCmux := filepath.Join(dir, "cmux")
	if err := os.WriteFile(fakeCmux, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$AMQ_KEEPALIVE_CMUX_CALLS"
printf '%s\n' '{"windows":[{"workspaces":[{"panes":[{"surfaces":[{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"ttys101"},{"id":"B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2","tty":"ttys102"}]}]}]}]}'
`), 0o700); err != nil {
		t.Fatalf("write fake cmux: %v", err)
	}
	t.Setenv("CMUX_BUNDLED_CLI_PATH", fakeCmux)
	wake := &appCountingWake{}
	adapters := adapter.NewRegistry(adapter.Cmux{
		Path: fakeCmux,
		LiveTTYOwnerCount: func(string) (int, error) {
			return 1, nil
		},
	}.WithOwnershipRecord())
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Adapters: &adapters}
	results, err := app.superviseOnce(context.Background(), registryPath, wake, "/bin/amq-keepalive", time.Second)
	if err != nil || len(results) != 2 || len(wake.starts) != 2 {
		t.Fatalf("first pass results=%#v starts=%d err=%v", results, len(wake.starts), err)
	}
	results, err = app.superviseOnce(context.Background(), registryPath, wake, "/bin/amq-keepalive", time.Second)
	if err != nil || len(results) != 2 || len(wake.starts) != 2 {
		t.Fatalf("deferred pass results=%#v starts=%d err=%v", results, len(wake.starts), err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read cmux calls: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(data)), "system.tree"); got != 1 {
		t.Fatalf("system.tree calls = %d, want one across two entries and one deferred pass:\n%s", got, data)
	}
}

func TestGCDryRunIsNonMutatingAndApplyRetiresCandidates(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	target := "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"
	store := registry.New(registryPath)
	entry, err := store.Upsert(registry.Entry{
		Root: "/tmp/old-session", Agent: "codex", Adapter: "cmux", Target: target,
		State: registry.StateDetached, DetachedSince: time.Now().Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	calls := filepath.Join(dir, "cmux-calls.log")
	t.Setenv("AMQ_KEEPALIVE_CMUX_CALLS", calls)
	fakeCmux := filepath.Join(dir, "cmux")
	if err := os.WriteFile(fakeCmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AMQ_KEEPALIVE_CMUX_CALLS\"\nprintf '%s\\n' '{\"windows\":[]}'\n"), 0o700); err != nil {
		t.Fatalf("write fake cmux: %v", err)
	}
	t.Setenv("CMUX_BUNDLED_CLI_PATH", fakeCmux)
	var dry bytes.Buffer
	code := (App{Stdout: &dry, Stderr: &bytes.Buffer{}}).Run(context.Background(), []string{
		"gc", "--registry", registryPath, "--min-detached-age", "0",
	})
	if code != 0 || !strings.Contains(dry.String(), `"status": "candidate"`) || !strings.Contains(dry.String(), `"applied": false`) {
		t.Fatalf("dry-run code=%d output=%s", code, dry.String())
	}
	loaded, err := store.Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0].ID != entry.ID {
		t.Fatalf("dry-run mutated registry: entries=%#v err=%v", loaded.Entries, err)
	}

	amqCalls := filepath.Join(dir, "amq-calls.log")
	t.Setenv("AMQ_KEEPALIVE_AMQ_CALLS", amqCalls)
	fakeAMQ := filepath.Join(dir, "amq")
	if err := os.WriteFile(fakeAMQ, fakeAMQWakeCheckThen(`
printf '%s\n' "$*" >> "$AMQ_KEEPALIVE_AMQ_CALLS"
printf '%s\n' '{"status":"retired","agent":"codex","pid":4242}'
`), 0o700); err != nil {
		t.Fatalf("write fake amq: %v", err)
	}
	var applied bytes.Buffer
	var applyErr bytes.Buffer
	code = (App{Stdout: &applied, Stderr: &applyErr}).Run(context.Background(), []string{
		"gc", "--registry", registryPath, "--min-detached-age", "0", "--apply",
		"--amq", fakeAMQ, "--self", "/bin/amq-keepalive",
	})
	if code != 0 || !strings.Contains(applied.String(), `"status": "retired"`) {
		t.Fatalf("apply code=%d stdout=%s stderr=%s", code, applied.String(), applyErr.String())
	}
	loaded, err = store.Load()
	if err != nil || len(loaded.Entries) != 0 {
		t.Fatalf("apply did not forget the retired entry: entries=%#v err=%v", loaded.Entries, err)
	}
	if data, err := os.ReadFile(amqCalls); err != nil || !strings.Contains(string(data), "wake retire") {
		t.Fatalf("gc apply did not invoke amq wake retire: data=%q err=%v", string(data), err)
	}
	data, err := os.ReadFile(calls)
	if err != nil || strings.Count(strings.TrimSpace(string(data)), "system.tree") != 2 {
		t.Fatalf("cmux inventory calls = %q err=%v, want one probe per gc invocation", data, err)
	}
}

func TestGCApplyAndReattachSerializeNewGenerationSurvives(t *testing.T) {
	dir := t.TempDir()
	registryPath := testRegistryPath(t, dir)
	liveRoot := filepath.Join(dir, "live-root")
	staleRoot := filepath.Join(dir, "stale-root")
	for _, root := range []string{liveRoot, staleRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	presence := &presenceAdapter{name: "boundary", present: map[string]bool{}}
	adapters := adapter.NewRegistry(presence)
	store := registry.New(registryPath)
	live, err := store.Upsert(registry.Entry{
		Root: liveRoot, Agent: "codex", Adapter: "boundary", Target: "live-surface",
		State: registry.StateDetached, DetachedSince: time.Now().Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Upsert(live): %v", err)
	}
	stale, err := store.Upsert(registry.Entry{
		Root: staleRoot, Agent: "stale-agent", Adapter: "boundary", Target: "stale-surface",
		State: registry.StateDetached, DetachedSince: time.Now().Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Upsert(stale): %v", err)
	}

	amqCalls := filepath.Join(dir, "amq-calls.log")
	t.Setenv("AMQ_KEEPALIVE_AMQ_CALLS", amqCalls)
	fakeAMQ := filepath.Join(dir, "amq")
	if err := os.WriteFile(fakeAMQ, fakeStartWakeScript(`#!/bin/sh
me=""
ready=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "--me" ] || [ "$previous" = "-me" ]; then me="$arg"; fi
  if [ "$previous" = "-ready-file" ]; then ready="$arg"; fi
  previous="$arg"
done
if [ "$1" = "wake" ] && [ "$2" = "check" ]; then
  printf '%s\n' '{"schema":1,"live_wake":true,"image_status":"current","wake_generation":"0123456789abcdef0123456789abcdef"}'
  exit 0
fi
if [ "$1" = "wake" ] && [ "$2" = "retire" ]; then
  printf 'RETIRE %s\n' "$me" >> "$AMQ_KEEPALIVE_AMQ_CALLS"
  sleep 0.05
  printf '%s\n' '{"status":"retired","agent":"'"$me"'","pid":1}'
  exit 0
fi
printf 'START %s\n' "$me" >> "$AMQ_KEEPALIVE_AMQ_CALLS"
[ -n "$ready" ] || exit 11
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
`), 0o700); err != nil {
		t.Fatalf("write fake amq: %v", err)
	}

	start := make(chan struct{})
	reattachHoldsLock := make(chan struct{})
	continueReattach := make(chan struct{})
	reattachDone := make(chan struct{})
	gcLoad := make(chan struct{})
	var gcLoadOnce sync.Once
	var releaseReattach sync.Once
	releaseContinueReattach := func() {
		releaseReattach.Do(func() { close(continueReattach) })
	}
	defer releaseContinueReattach()
	t.Cleanup(func() {
		afterReattachRegistrationLockHeldForTest = nil
		beforeGCRegistryLoadForTest = nil
	})
	afterReattachRegistrationLockHeldForTest = func() {
		select {
		case <-reattachHoldsLock:
		default:
			close(reattachHoldsLock)
		}
		<-continueReattach
		// Presence stays absent through the lock-held proof so gc cannot skip
		// on a live probe. Reattach itself still needs the target after that.
		presence.setPresent("live-surface")
	}
	beforeGCRegistryLoadForTest = func() {
		select {
		case <-reattachDone:
		default:
			t.Errorf("gc loaded the registry before reattach released the registration lock")
		}
		gcLoadOnce.Do(func() { close(gcLoad) })
	}

	type outcome struct {
		name string
		code int
		out  string
		err  string
	}
	outcomes := make(chan outcome, 2)
	go func() {
		<-start
		var stdout, stderr bytes.Buffer
		code := (App{Stdout: &stdout, Stderr: &stderr, Adapters: &adapters}).Run(context.Background(), []string{
			"reattach", "--registry", registryPath, "--adapter", "boundary", "--target", "live-surface",
			"--root", liveRoot, "--base-root", dir, "--session", "live", "--me", "codex",
			"--amq", fakeAMQ, "--self", "/bin/amq-keepalive",
		})
		outcomes <- outcome{name: "reattach", code: code, out: stdout.String(), err: stderr.String()}
	}()
	go func() {
		<-reattachHoldsLock
		var stdout, stderr bytes.Buffer
		code := (App{Stdout: &stdout, Stderr: &stderr, Adapters: &adapters}).Run(context.Background(), []string{
			"gc", "--registry", registryPath, "--min-detached-age", "0", "--apply",
			"--amq", fakeAMQ, "--self", "/bin/amq-keepalive",
		})
		outcomes <- outcome{name: "gc", code: code, out: stdout.String(), err: stderr.String()}
	}()
	close(start)
	select {
	case <-reattachHoldsLock:
	case <-time.After(5 * time.Second):
		t.Fatal("reattach did not acquire the registration lock")
	}
	close(reattachDone)
	releaseContinueReattach()
	first, second := <-outcomes, <-outcomes
	var reattachOut, gcOut outcome
	for _, item := range []outcome{first, second} {
		switch item.name {
		case "reattach":
			reattachOut = item
		case "gc":
			gcOut = item
		}
	}
	if reattachOut.code != 0 {
		t.Fatalf("reattach code=%d stdout=%s stderr=%s", reattachOut.code, reattachOut.out, reattachOut.err)
	}
	if gcOut.code != 0 {
		t.Fatalf("gc code=%d stdout=%s stderr=%s", gcOut.code, gcOut.out, gcOut.err)
	}
	select {
	case <-gcLoad:
	default:
		t.Fatal("gc did not reach the registry load barrier")
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	foundLive, foundStale := false, false
	for _, entry := range loaded.Entries {
		switch entry.ID {
		case live.ID:
			foundLive = true
			if entry.State != registry.StateActive && entry.State != registry.StateAttached {
				t.Fatalf("live generation state=%q, want attached/active: %#v", entry.State, entry)
			}
		case stale.ID:
			foundStale = true
		}
	}
	if !foundLive {
		t.Fatalf("new live generation was forgotten: entries=%#v", loaded.Entries)
	}
	if foundStale {
		t.Fatalf("stale detached entry was not retired: entries=%#v", loaded.Entries)
	}

	data, err := os.ReadFile(amqCalls)
	if err != nil {
		t.Fatalf("read amq log: %v", err)
	}
	startedLive := false
	for _, line := range strings.Split(string(data), "\n") {
		switch line {
		case "START codex":
			startedLive = true
		case "RETIRE codex":
			if startedLive {
				t.Fatalf("gc retired G2 after reattach started it: log=%q", data)
			}
		}
	}
	if !startedLive {
		t.Fatalf("reattach did not start G2: log=%q gc=%s", data, gcOut.out)
	}
}

type boundaryAdapter struct {
	name     string
	probeErr error
}

func (a boundaryAdapter) Name() string { return a.name }

// boundaryAdapter is a stand-in for a real adapter in behavioral tests, so it
// declares the full-strength unattended vector real TTY/file seats declare.
func (boundaryAdapter) Capability() adapter.Capability {
	return adapter.Capability{
		Activation:    adapter.ActivationNone,
		Delivery:      adapter.DeliverySubmitted,
		Session:       adapter.SessionExistingExact,
		RequiresHuman: false,
	}
}

func (a boundaryAdapter) Probe(context.Context, string) error { return a.probeErr }

func (a boundaryAdapter) Inject(context.Context, string, string) error { return nil }

type presenceAdapter struct {
	name    string
	mu      sync.Mutex
	present map[string]bool
}

func (a *presenceAdapter) Name() string { return a.name }

// presenceAdapter is a stand-in for a real adapter in presence/inventory
// tests, so it declares the full-strength unattended vector real seats declare.
func (*presenceAdapter) Capability() adapter.Capability {
	return adapter.Capability{
		Activation:    adapter.ActivationNone,
		Delivery:      adapter.DeliverySubmitted,
		Session:       adapter.SessionExistingExact,
		RequiresHuman: false,
	}
}

func (a *presenceAdapter) Probe(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.present[target] {
		return nil
	}
	return adapter.ErrTargetNotFound
}

func (a *presenceAdapter) Inject(context.Context, string, string) error { return nil }

func (a *presenceAdapter) setPresent(target string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.present == nil {
		a.present = map[string]bool{}
	}
	a.present[target] = true
}

func TestObserveWakeGenerationRefusesLiveWakeWithoutGeneration(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name    string
		check   amq.WakeCheckResult
		wantErr bool
		want    string
	}{
		{name: "live omitted", check: amq.WakeCheckResult{LiveWake: true}, wantErr: true},
		{name: "live present", check: amq.WakeCheckResult{LiveWake: true, Generation: generation}, want: generation},
		{name: "not live omitted", check: amq.WakeCheckResult{LiveWake: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			retirer := &recordingRetirer{check: tc.check}
			got, err := observeWakeGeneration(context.Background(), retirer, "/tmp/root", "codex")
			if tc.wantErr {
				if err == nil || got != "" || retirer.retireCalls != 0 {
					t.Fatalf("got=%q err=%v retire=%d, want refuse with no retire", got, err, retirer.retireCalls)
				}
				return
			}
			if err != nil || got != tc.want || retirer.retireCalls != 0 {
				t.Fatalf("got=%q err=%v retire=%d, want %q with no retire", got, err, retirer.retireCalls, tc.want)
			}
		})
	}
}

type recordingRetirer struct {
	check       amq.WakeCheckResult
	retireCalls int
}

func (r *recordingRetirer) CheckWake(context.Context, amq.StartWakeRequest) (amq.WakeCheckResult, error) {
	return r.check, nil
}

func (r *recordingRetirer) RetireWake(context.Context, amq.RetireWakeRequest) (amq.RetireWakeResult, error) {
	r.retireCalls++
	return amq.RetireWakeResult{}, nil
}

func TestRegisterUsesEnvBaseRootAndSessionIndependently(t *testing.T) {
	dir := t.TempDir()
	envBase := filepath.Join(dir, ".agent-mail")
	expectedRoot := filepath.Join(envBase, "session")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeAMQ := filepath.Join(dir, "amq")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '{\"root\":\"ignored\",\"base_root\":%q,\"session_name\":\"session\",\"me\":\"env-agent\"}'\n", envBase)
	if err := os.WriteFile(fakeAMQ, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{"base", "session"} {
		t.Run(missing, func(t *testing.T) {
			registryPath := testRegistryTempPath(t)
			opts := registerOptions{
				RegistryPath: registryPath,
				AdapterName:  "file",
				Target:       target,
				Root:         ".agent-mail/session",
				BaseRoot:     envBase,
				SessionName:  "session",
				Me:           "codex",
				AMQPath:      fakeAMQ,
				NoStart:      true,
			}
			if missing == "base" {
				opts.BaseRoot = ""
			} else {
				opts.SessionName = ""
			}
			if err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).registerWithOptions(context.Background(), opts); err != nil {
				t.Fatalf("registerWithOptions: %v", err)
			}
			loaded, err := registry.New(registryPath).Load()
			if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0].Root != expectedRoot {
				t.Fatalf("entries=%#v err=%v, want root %q", loaded.Entries, err, expectedRoot)
			}
		})
	}
}

func TestSuperviseOnceNoSelfUpgradePublishesJSONWithoutState(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(dir, "registry.json")
	var stdout, stderr bytes.Buffer
	code := (App{Stdout: &stdout, Stderr: &stderr}).Run(
		context.Background(),
		[]string{"supervise", "--registry", registryPath, "--once", "--no-self-upgrade"},
	)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q, want successful one-shot pass", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "[]" {
		t.Fatalf("stdout=%q, want empty JSON results", got)
	}
}

func TestRetireSessionMatchesRootAdapterAndAgentTogether(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	correct := registry.Entry{
		ID: registry.EntryID(root, "codex", "boundary", "correct"), Root: root, Agent: "codex", Adapter: "boundary", Target: "correct",
	}
	wrongAdapter := registry.Entry{
		ID: registry.EntryID(root, "codex", "other", "wrong-adapter"), Root: root, Agent: "codex", Adapter: "other", Target: "wrong-adapter",
	}
	wrongAgent := registry.Entry{
		ID: registry.EntryID(root, "claude", "boundary", "wrong-agent"), Root: root, Agent: "claude", Adapter: "boundary", Target: "wrong-agent",
	}
	registryPath := testRegistryPath(t, dir)
	if err := registry.New(registryPath).Save(registry.File{Entries: []registry.Entry{correct, wrongAdapter, wrongAgent}}); err != nil {
		t.Fatal(err)
	}
	fakeAMQ := filepath.Join(dir, "amq")
	if err := os.WriteFile(fakeAMQ, fakeAMQWakeCheckThen("printf '%s\\n' '{\"status\":\"retired\",\"pid\":42}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	adapters := adapter.NewRegistry(
		boundaryAdapter{name: "boundary", probeErr: adapter.ErrTargetNotFound},
		boundaryAdapter{name: "other", probeErr: adapter.ErrTargetNotFound},
	)
	var stderr bytes.Buffer
	code := (App{Stdout: &bytes.Buffer{}, Stderr: &stderr, Adapters: &adapters}).Run(context.Background(), []string{
		"retire-session", "--registry", registryPath, "--root", root, "--adapter", "boundary",
		"--agents", "codex", "--amq", fakeAMQ, "--self", "/bin/echo",
	})
	if code != 0 {
		t.Fatalf("retire-session code=%d stderr=%q", code, stderr.String())
	}
	loaded, err := registry.New(registryPath).Load()
	if err != nil || len(loaded.Entries) != 2 {
		t.Fatalf("entries=%#v err=%v, want only exact match removed", loaded.Entries, err)
	}
	for _, entry := range loaded.Entries {
		if entry.ID == correct.ID {
			t.Fatalf("exact entry remained: %#v", loaded.Entries)
		}
	}
}

func testRegistryPath(t *testing.T, dir string) string {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod registry test dir: %v", err)
	}
	return filepath.Join(dir, "registry.json")
}

func testRegistryTempPath(t *testing.T) string {
	t.Helper()
	return testRegistryPath(t, t.TempDir())
}

func runApp(t *testing.T, args ...string) {
	t.Helper()
	runAppWith(t, App{}, args...)
}

func runAppWith(t *testing.T, app App, args ...string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app.Stdout = &stdout
	app.Stderr = &stderr
	code := app.Run(context.Background(), args)
	if code != 0 {
		t.Fatalf("Run(%v) = %d\nstdout:\n%s\nstderr:\n%s", args, code, stdout.String(), stderr.String())
	}
}

func hermeticCmuxRegistry(fakeCmuxPath string) adapter.Registry {
	return adapter.NewRegistry(adapter.File{}, adapter.Ghostty{}, adapter.Cmux{
		Path: fakeCmuxPath,
		LiveTTYOwnerCount: func(string) (int, error) {
			return 1, nil
		},
	}.WithOwnershipRecord())
}

func fakeStartWakeScript(body string) []byte {
	return fakeAMQWakeCheckThen(body)
}

func fakeAMQWakeCheckThen(body string) []byte {
	body = strings.TrimPrefix(body, "#!/bin/sh\n")
	return []byte(`#!/bin/sh
if [ "$1" = "wake" ] && [ "$2" = "check" ]; then
  printf '%s\n' '{"schema":1,"live_wake":true,"image_status":"current","wake_generation":"0123456789abcdef0123456789abcdef"}'
  exit 0
fi
` + body)
}

func normalizedFileTarget(t *testing.T, target string) string {
	t.Helper()
	normalized, err := (adapter.File{}).NormalizeTarget(target)
	if err != nil {
		t.Fatalf("NormalizeTarget(%q): %v", target, err)
	}
	return normalized
}

// Restored per codex review of PR #724: PR #706 fixed an observed defect where
// standalone uncertainty markers were not emitted and duplicate markers survived
// in diagnostics. This is the cited-regression proof; surviving acceptance tests
// do not cover it.
func TestInjectUncertainPrintsStandaloneMarkerAndSeparateDiagnostics(t *testing.T) {
	selected := uncertainInjectProgressAdapter{
		injectProgressAdapter: injectProgressAdapter{name: "uncertain"},
	}
	adapters := adapter.NewRegistry(selected)
	var stderr bytes.Buffer
	code := (App{Stdout: &bytes.Buffer{}, Stderr: &stderr, Adapters: &adapters}).Run(
		context.Background(),
		[]string{"inject", selected.Name(), "target", "payload"},
	)
	if code != 1 {
		t.Fatalf("inject code = %d, want 1; stderr = %q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) < 2 || lines[0] != adapter.ErrInjectUncertain.Error() {
		t.Fatalf("stderr = %q, want standalone uncertainty marker followed by diagnostics", stderr.String())
	}
	for _, line := range lines[1:] {
		if strings.Contains(line, adapter.ErrInjectUncertain.Error()) {
			t.Fatalf("diagnostic line %q repeats the machine-readable marker", line)
		}
	}
}
