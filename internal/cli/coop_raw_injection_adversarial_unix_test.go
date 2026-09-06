//go:build darwin || linux

package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const publicCoopPTYTimeout = 12 * time.Second

func TestCoopRawDoorbellDoesNotContainMessageDerivedBytes(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureAdversarialCoopMailbox(t, root, "codex")

	sentinels := []string{
		"SESSION_SENTINEL_$(touch /tmp/amq-session-pwned)",
		"HEADER_SENTINEL_\x1b[31m",
		"SUBJECT_SENTINEL_`id`",
		"BODY_SENTINEL_\x03_é",
	}
	message := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       "adversarial-terminal-payload",
			From:     sentinels[1],
			To:       []string{"codex"},
			Thread:   "p2p/attacker__codex",
			Subject:  sentinels[2],
			Created:  "2026-07-25T00:00:00Z",
			Priority: format.PriorityNormal,
		},
		Body: sentinels[3],
	}
	messagePath := deliverAdversarialWakeMessage(t, root, "codex", message)
	cleanupWake, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeRaw,
	})
	if err != nil {
		t.Fatalf("acquire adversarial raw wake: %v", err)
	}
	defer cleanupWake()

	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	t.Cleanup(func() {
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}

	var injected []string
	cfg := &wakeConfig{
		me:          "codex",
		root:        root,
		session:     sentinels[0],
		injectMode:  wakeInjectModeRaw,
		controlStop: make(chan struct{}),
		terminalWrite: func(chunk string) error {
			injected = append(injected, chunk)
			return nil
		},
		previewLen: 4096,
	}
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify adversarial pending message: %v", err)
	}

	assertFixedASCIIOnlyCoopInjection(t, injected, sentinels)
	if _, err := os.Stat(messagePath); err != nil {
		t.Fatalf("wake notification consumed durable message: %v", err)
	}
}

func assertFixedASCIIOnlyCoopInjection(t *testing.T, chunks, forbidden []string) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("coop raw wake injected no doorbell")
	}
	if !validCoopWakeDoorbell(chunks[0]) {
		t.Fatalf("first coop injection chunk = %q, want canonical fixed doorbell; all chunks=%#v",
			chunks[0], chunks)
	}
	joined := strings.Join(chunks, "")
	for index, value := range []byte(joined) {
		if value > 0x7f {
			t.Fatalf("coop injection byte %d = %#x, want fixed ASCII only: %#v", index, value, chunks)
		}
	}
	for _, sentinel := range forbidden {
		if strings.Contains(joined, sentinel) {
			t.Fatalf("coop injection leaked message-derived sentinel %q: %#v", sentinel, chunks)
		}
	}
}

func ensureAdversarialCoopMailbox(t *testing.T, root, me string) {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure adversarial root: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, me); err != nil {
		t.Fatalf("ensure adversarial mailbox: %v", err)
	}
}

func deliverAdversarialWakeMessage(t *testing.T, root, me string, message format.Message) string {
	t.Helper()
	data, err := message.Marshal()
	if err != nil {
		t.Fatalf("marshal adversarial message: %v", err)
	}
	path, err := deliverToInboxForTest(t, root, me, message.Header.ID+".md", data)
	if err != nil {
		t.Fatalf("deliver adversarial message: %v", err)
	}
	return path
}

type publicCoopPTYFixture struct {
	root           string
	ownerPath      string
	linePath       string
	interruptPath  string
	cmd            *exec.Cmd
	processGroup   int
	commandPIDPath string
	done           chan error
	output         *bytes.Buffer
	stdin          *os.File
	ownerPID       int
	wakeClaim      *wakeLockInspection
	waited         bool
}

func (fixture *publicCoopPTYFixture) wait() error {
	if fixture.waited {
		return nil
	}
	if fixture.stdin != nil {
		_ = fixture.stdin.Close()
		fixture.stdin = nil
	}
	timer := time.NewTimer(publicCoopPTYTimeout)
	defer timer.Stop()
	select {
	case err := <-fixture.done:
		fixture.waited = true
		return err
	case <-timer.C:
		if fixture.cmd.Process != nil {
			_ = fixture.cmd.Process.Kill()
		}
		select {
		case <-fixture.done:
			fixture.waited = true
		case <-time.After(3 * time.Second):
		}
		return fmt.Errorf("public coop PTY did not exit within %s", publicCoopPTYTimeout)
	}
}
