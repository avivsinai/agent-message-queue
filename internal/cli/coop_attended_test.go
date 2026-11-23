//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// The coop exec delivery line names an attended seat once, for every
// harness, with no prediction of what the hooks will deliver.
func TestPublishStartupDialogMarkerAttendedDeliveryLine(t *testing.T) {
	clearSendMailboxTestEnv(t)
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	for _, marker := range []wakeDialogMarker{
		{Schema: 1, Harness: "claude"},
		{Schema: 1, Harness: "codex"},
		{},
	} {
		_, stderr, err := captureEnvOutput(t, func() error {
			return publishStartupDialogMarker(h.root, "codex", marker, false, "")
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr, "dialog guard: plain\n") {
			t.Fatalf("stderr = %q, want the plain guard line", stderr)
		}
		if !strings.Contains(stderr, "delivery: attended (no doorbell is typed; mail shows as a notice)\n") {
			t.Fatalf("stderr = %q, want the universal attended delivery line", stderr)
		}
	}

	// A seat that is not attended prints nothing new.
	if err := os.Remove(filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName)); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := captureEnvOutput(t, func() error {
		return publishStartupDialogMarker(h.root, "codex", wakeDialogMarker{}, false, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "delivery: attended") {
		t.Fatalf("unattended stderr = %q, want no attended delivery line", stderr)
	}
}

// Review finding H16: coop exec --no-wake printed no delivery line for a
// seat with hook_doorbell on, although its waiter is then the only ringer.
func TestCoopExecNoWakePrintsHookDoorbellDeliveryLine(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := secureTempDirForTest(t)
	if err := runInit([]string{"--root", root, "--agents", "codex"}); err != nil {
		t.Fatal(err)
	}
	settings := []byte(`{"schema":1,"settings":{"hook_doorbell":true}}`)
	if err := os.WriteFile(filepath.Join(fsq.AgentBase(root, "codex"), wakeSettingsFileName), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := captureCoopExecStderr(t, []string{"--root", root, "--me", "codex", "--no-wake", "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "delivery: hook doorbell (the harness's hook rings; the wake types only when no hook waits)\n") {
		t.Fatalf("stderr = %q, want the hook doorbell delivery line", stderr)
	}

	// A live notify-only wake publishes no hook channel, so the terminal
	// waiter does not run; the line said the hook rings all the same.
	process := inspectWakeProcess(os.Getpid())
	process.Executable = "/opt/amq"
	process.Args = []string{"amq", "wake"}
	stubInspectWakeProcess(t, func(int) wakeProcessInfo { return process })
	stopWake, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{wakeMode: wakeInjectModeNone})
	if err != nil {
		t.Fatal(err)
	}
	defer stopWake()
	_, stderr, err = captureEnvOutput(t, func() error {
		writeSeatDeliveryLine(root, "codex")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "delivery: hook doorbell") {
		t.Fatalf("notify-only wake: stderr = %q, want no hook doorbell line", stderr)
	}
}

// A waiter on an attended seat stays quiet for its whole window and
// records no rang ids, as it does on a quiet inbox: the gate in
// waitCoopMail suppresses the ring, so a removed gate fails this test.
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
	if rang := coopAnnounced(filepath.Join(root, "agents", "claude"), claudeSession, false).rang; len(rang.RangIDs) != 0 {
		t.Fatalf("attended waiter rang %v, want no rang ids", rang.RangIDs)
	}
}
