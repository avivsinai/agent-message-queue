package cli

// Purpose: the attended seat's hooks and commands surface (bead
// agent-message-queue-gijx). The Stop guard of an attended seat reports once
// per message with a systemMessage and never blocks; a seat that is not
// attended keeps its block. The wait hook of an attended seat does not ring.
// `amq wake config --attended=true` works on a seat with no live wake and
// the hooks read it back. The coop exec delivery line names the attended
// seat, with or without the co-op hooks.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func sendCodexToClaudeForAttendedTest(t *testing.T, root string) {
	t.Helper()
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "codex", "--to", "claude", "--body", "hi"})
	}); err != nil {
		t.Fatal(err)
	}
}

// An attended seat's Stop guard prints the systemMessage once per fresh
// message and never `decision: block`; a repeat at the next turn end prints
// nothing. A seat that is not attended keeps its block (owner ruling R2).
func TestCoopHookStopOnAttendedSeatReportsAndNeverBlocks(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	root := filepath.Join(base, "collab")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	sendCodexToClaudeForAttendedTest(t, root)
	t.Setenv(envMe, "claude")
	t.Setenv(envRoot, root)
	t.Setenv(envBaseRoot, base)
	t.Setenv(envSession, "collab")
	setAttendedForTest(t, root, "claude")

	stop := func(active string) string {
		t.Helper()
		var out bytes.Buffer
		if err := writeCoopStop(strings.NewReader(`{"stop_hook_active":`+active+`}`), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first := stop("false")
	if strings.Contains(first, `"decision"`) {
		t.Fatalf("attended stop = %q, want no block decision", first)
	}
	if !strings.Contains(first, "systemMessage") || !strings.Contains(first, "amq drain") {
		t.Fatalf("attended stop = %q, want the drain systemMessage", first)
	}
	if got := stop("true"); got != "" {
		t.Fatalf("attended repeat stop = %q, want silence", got)
	}
	// A second message is reported once too.
	sendCodexToClaudeForAttendedTest(t, root)
	second := stop("false")
	if strings.Contains(second, `"decision"`) || !strings.Contains(second, "systemMessage") {
		t.Fatalf("attended stop for new mail = %q, want a systemMessage and no block", second)
	}
}

// The Stop guard of a seat that is not attended still blocks (R2).
func TestCoopHookStopWithoutAttendedKeepsTheBlock(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	root := filepath.Join(base, "collab")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	sendCodexToClaudeForAttendedTest(t, root)
	t.Setenv(envMe, "claude")
	t.Setenv(envRoot, root)
	t.Setenv(envBaseRoot, base)
	t.Setenv(envSession, "collab")

	var out bytes.Buffer
	if err := writeCoopStop(strings.NewReader(`{"stop_hook_active":false}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"decision":"block"`) {
		t.Fatalf("unattended stop = %q, want the block", out.String())
	}
}

// An attended seat's waiter does not ring, while the same seat unattended
// does (bead gijx, addendum I).
func TestCoopWaitOnAttendedSeatDoesNotRing(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	root = canonicalWakeRoot(root)
	const claudeSession = "7c29e148-77eb-47c0-8984-10c13b4700b1"
	setAttendedForTest(t, root, "claude")
	sendCodexToClaudeForAttendedTest(t, root)

	// wait runs one waiter for at most limit. With pending mail and
	// attended=true it must stay quiet for the whole window and record no
	// rang ids, as the unattended test's quiet inbox does.
	quiet := 2 * defaultWakeDebounce
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), quiet)
	defer cancel()
	err := waitCoopMail(ctx, root, "claude", claudeWaiter(claudeSession, os.Getppid(), launch.HolderIdentity{}))
	if errors.Is(err, errCoopWaitRing) {
		t.Fatalf("attended waiter rang: %v, want no ring", err)
	}
	if err != nil {
		t.Fatalf("attended waiter: %v, want a quiet end", err)
	}
	if elapsed := time.Since(start); elapsed < quiet/2 {
		t.Fatalf("attended waiter ended after %s, want it to run its whole %s window", elapsed, quiet)
	}
	if rang := readCoopWaitState(filepath.Join(root, "agents", "claude", coopWaitStateFile), claudeSession); len(rang.RangIDs) != 0 {
		t.Fatalf("attended waiter rang %v, want no rang ids", rang.RangIDs)
	}
}

// `amq wake config --attended=true` works on a seat with no live wake: it
// writes the agent's settings file, and coopWaitSettings reads it (addendum
// G). An invalid value is refused as any setting is.
func TestWakeConfigAttendedOnSeatWithoutAWake(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents", "claude", "inbox", "new"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return runWakeConfig([]string{"--root", root, "--me", "claude", "--attended=true", "--json"})
	})
	if err != nil {
		t.Fatalf("wake config --attended on a seat with no wake: %v", err)
	}
	var got wakeConfigJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if s := got.Settings["attended"]; s.Value != true || s.Source != "file" {
		t.Fatalf("attended = %+v, want true from file", s)
	}
	settings, _ := coopWaitSettings(canonicalWakeRoot(root), "claude", wakeSettingsLayers{})
	if !settings.attended {
		t.Fatal("coopWaitSettings did not read attended from the seat with no wake")
	}
}

// The coop exec delivery line names an attended seat once, with or without
// the co-op hooks installed.
func TestPublishStartupDialogMarkerAttendedDeliveryLine(t *testing.T) {
	clearSendMailboxTestEnv(t)
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	_, stderr, err := captureEnvOutput(t, func() error {
		return publishStartupDialogMarker(h.root, "codex", wakeDialogMarker{}, false, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "dialog guard: plain\n") {
		t.Fatalf("stderr = %q, want the plain guard line", stderr)
	}
	if !strings.Contains(stderr, "delivery: attended (notice only; run amq coop install-hooks for context at your next prompt)\n") {
		t.Fatalf("stderr = %q, want the notice-only attended delivery line", stderr)
	}

	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	hooks := `{"hooks":{"Stop":[{"hooks":[{"command":"` + coopHookMarker + `amq coop hook stop || true"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = captureEnvOutput(t, func() error {
		return publishStartupDialogMarker(h.root, "codex", wakeDialogMarker{}, false, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "delivery: attended (notice, and context at your next prompt)\n") {
		t.Fatalf("stderr = %q, want the context attended delivery line", stderr)
	}

	// A seat that is not attended prints nothing new.
	if err := os.Remove(filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName)); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = captureEnvOutput(t, func() error {
		return publishStartupDialogMarker(h.root, "codex", wakeDialogMarker{}, false, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "delivery: attended") {
		t.Fatalf("unattended stderr = %q, want no attended delivery line", stderr)
	}
}
