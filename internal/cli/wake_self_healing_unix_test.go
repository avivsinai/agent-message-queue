//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func stubFastWakeInboxRetry(t *testing.T) {
	t.Helper()
	originalBase := wakeInboxScanRetryBase
	originalMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = 10 * time.Millisecond
	wakeInboxScanRetryMax = 20 * time.Millisecond
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalBase
		wakeInboxScanRetryMax = originalMax
	})
}

func TestRunWakeLoopRecoveryProgressStartsFreshDoorbell(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverWakeWatcherMessageForTest(t, root, "codex", "first", "claude")
	deliverWakeWatcherMessageForTest(t, root, "codex", "second", "claude")

	firstPath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "first.md")
	secondPath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "second.md")
	firstInfo, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	cohort := map[string]os.FileInfo{
		"first.md":  firstInfo,
		"second.md": secondInfo,
	}

	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	})
	stubRawInjectSleep(t)

	writes := make(chan string, 8)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:                  root,
			me:                    "codex",
			session:               "session1",
			wakeOwner:             &wakeOwner{},
			debounce:              5 * time.Millisecond,
			injectMode:            wakeInjectModeRaw,
			controlStop:           stop,
			inputRecoveryRequired: true,
			inputDelivery: wakeInputDeliveryState{
				phase:         wakeInputPayloadPending,
				mode:          wakeInjectModeRaw,
				payload:       coopWakeDoorbell,
				acceptedBytes: 7,
			},
			doorbell: wakeDoorbellState{
				phase:       wakeDoorbellRecoveryRequired,
				cohort:      snapshotWakeFileIdentities(cohort),
				attempts:    1,
				nextAttempt: time.Now().Add(time.Hour),
			},
			terminalWrite: func(data string) error {
				writes <- data
				return nil
			},
			attentionIsTTY: func() bool { return false },
		})
	}()

	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	if err := os.Remove(firstPath); err != nil {
		t.Fatal(err)
	}
	select {
	case write := <-writes:
		if write != coopWakeDoorbell {
			t.Fatalf("post-progress write = %q, want fresh complete doorbell", write)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before fresh recovery doorbell: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("consumer progress left wake permanently recovery-required")
	}
}
