//go:build darwin || linux

package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	adversarialCoopDoorbell = ": AMQ doorbell run amq drain --include-body then act on it"
	publicCoopPTYTimeout    = 12 * time.Second
)

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

func TestCoopUrgentRefusalInjectsNoCtrlCAndKeepsMessagePending(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureAdversarialCoopMailbox(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       "urgent-refused-adversarial",
			From:     "CTRL_C_SENTINEL_\x03",
			To:       []string{"codex"},
			Thread:   "p2p/attacker__codex",
			Subject:  "URGENT_SENTINEL_$(kill -INT $$)",
			Created:  "2026-07-25T00:00:00Z",
			Priority: format.PriorityUrgent,
			Labels:   []string{"interrupt"},
		},
		Body: "BODY_SENTINEL_must_remain_pending",
	}
	messagePath := deliverAdversarialWakeMessage(t, root, "codex", message)

	var injected []string

	stop := make(chan struct{})
	close(stop)
	cfg := &wakeConfig{
		me:                "codex",
		root:              root,
		session:           "SESSION_SENTINEL",
		injectMode:        wakeInjectModeRaw,
		controlStop:       stop,
		previewLen:        4096,
		interrupt:         true,
		interruptKey:      "\x03",
		interruptLabel:    "interrupt",
		interruptPriority: format.PriorityUrgent,
		terminalWrite: func(chunk string) error {
			injected = append(injected, chunk)
			return nil
		},
	}
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify refused urgent message: %v", err)
	}
	if len(injected) != 0 {
		t.Fatalf("refused urgent wake injected terminal chunks: %#v", injected)
	}
	if strings.Contains(strings.Join(injected, ""), "\x03") {
		t.Fatalf("refused urgent wake injected Ctrl-C: %#v", injected)
	}
	if _, err := os.Stat(messagePath); err != nil {
		t.Fatalf("refused urgent notification consumed durable message: %v", err)
	}
}

func TestPublicCoopRawPTYDoorbellAndOwnerCleanup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the hermetic public raw PTY oracle currently uses Darwin /usr/bin/script argv semantics")
	}
	if _, err := os.Stat("/usr/bin/script"); err != nil {
		t.Skipf("public raw PTY oracle requires /usr/bin/script: %v", err)
	}

	t.Run("normal owner exit", func(t *testing.T) {
		fixture := startPublicCoopPTYFixture(t, "capture")
		inspection := waitForPublicCoopWakeReady(t, fixture)

		sentinels := []string{
			"PUBLIC_SESSION_SENTINEL",
			"PUBLIC_HEADER_SENTINEL_\x1b[31m",
			"PUBLIC_SUBJECT_SENTINEL_$(touch /tmp/amq-public-pwned)",
			"PUBLIC_BODY_SENTINEL_\x03_é",
		}
		message := format.Message{
			Header: format.Header{
				Schema:   format.CurrentSchema,
				ID:       "public-raw-adversarial",
				From:     sentinels[1],
				To:       []string{"codex"},
				Thread:   "p2p/attacker__codex",
				Subject:  sentinels[2],
				Created:  "2026-07-25T00:00:00Z",
				Priority: format.PriorityNormal,
			},
			Body: sentinels[3],
		}
		messagePath := deliverAdversarialWakeMessage(t, fixture.root, "codex", message)

		waitForAdversarialPath(t, fixture.linePath, publicCoopPTYTimeout)
		line, err := os.ReadFile(fixture.linePath)
		if err != nil {
			t.Fatalf("read public PTY input: %v", err)
		}
		if got := strings.TrimSuffix(string(line), "\r"); got != adversarialCoopDoorbell {
			t.Fatalf("public PTY input = %q, want fixed doorbell %q", got, adversarialCoopDoorbell)
		}
		for _, sentinel := range sentinels {
			if bytes.Contains(line, []byte(sentinel)) {
				t.Fatalf("public PTY input leaked %q: %q", sentinel, line)
			}
		}
		if bytes.Contains(line, []byte{0x03}) {
			t.Fatalf("public PTY input contains Ctrl-C: %q", line)
		}
		if _, err := os.Stat(fixture.interruptPath); !os.IsNotExist(err) {
			t.Fatalf("public coop owner received SIGINT: %v", err)
		}
		if _, err := os.Stat(messagePath); err != nil {
			t.Fatalf("public wake consumed durable message: %v", err)
		}

		if err := fixture.wait(); err != nil {
			t.Fatalf("public coop normal exit: %v\n%s", err, fixture.output.String())
		}
		assertPublicWakeCleaned(t, fixture.root, "codex", inspection.PID)
	})

	t.Run("SIGKILL owner exit", func(t *testing.T) {
		fixture := startPublicCoopPTYFixture(t, "hold")
		inspection := waitForPublicCoopWakeReady(t, fixture)
		ownerPID := fixture.ownerPID

		if err := syscall.Kill(ownerPID, syscall.SIGKILL); err != nil {
			t.Fatalf("SIGKILL public coop owner %d: %v", ownerPID, err)
		}
		// The PTY wrapper reports the killed child as a non-zero exit. The wake
		// cleanup, not the wrapper exit code, is the public contract under test.
		_ = fixture.wait()
		assertPublicWakeCleaned(t, fixture.root, "codex", inspection.PID)
	})
}

func assertFixedASCIIOnlyCoopInjection(t *testing.T, chunks, forbidden []string) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("coop raw wake injected no doorbell")
	}
	if chunks[0] != adversarialCoopDoorbell {
		t.Fatalf("first coop injection chunk = %q, want %q; all chunks=%#v",
			chunks[0], adversarialCoopDoorbell, chunks)
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
	root          string
	ownerPath     string
	linePath      string
	interruptPath string
	cmd           *exec.Cmd
	done          chan error
	output        *bytes.Buffer
	stdin         *os.File
	ownerPID      int
	wakeClaim     *wakeLockInspection
	waited        bool
}

func startPublicCoopPTYFixture(t *testing.T, mode string) *publicCoopPTYFixture {
	t.Helper()
	root := secureTempDirForTest(t)
	ensureAdversarialCoopMailbox(t, root, "codex")
	dir := secureTempDirForTest(t)
	ownerPath := filepath.Join(dir, "owner.pid")
	linePath := filepath.Join(dir, "terminal-line")
	interruptPath := filepath.Join(dir, "interrupt")
	agentPath := filepath.Join(dir, "agent.sh")
	agentScript := `#!/bin/sh
set -eu
owner_path=$1
line_path=$2
interrupt_path=$3
mode=$4
printf '%s\n' "$$" > "$owner_path"
trap 'printf "SIGINT\n" > "$interrupt_path"; exit 97' INT
if [ "$mode" = capture ]; then
	IFS= read -r line
	line_tmp="${line_path}.tmp.$$"
	printf '%s' "$line" > "$line_tmp"
	mv "$line_tmp" "$line_path"
	exit 0
fi
while :; do
	sleep 1
done
`
	if err := os.WriteFile(agentPath, []byte(agentScript), 0o700); err != nil {
		t.Fatalf("write public coop agent: %v", err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	// Wake identity validation intentionally requires an executable named amq.
	// Preserve the compiled package-test entry point while giving the public
	// subprocess the same executable identity as a shipped binary.
	amqBinary := filepath.Join(dir, "amq")
	testData, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read compiled test binary: %v", err)
	}
	if err := os.WriteFile(amqBinary, testData, 0o700); err != nil {
		t.Fatalf("write hermetic amq binary: %v", err)
	}
	args := []string{
		"-q", "/dev/null",
		amqBinary,
		"--no-update-check",
		"coop", "exec",
		"--root", root,
		"--me", "codex",
		"--require-wake",
		"--wake-inject-mode", wakeInjectModeRaw,
		agentPath,
		ownerPath,
		linePath,
		interruptPath,
		mode,
	}
	output := &bytes.Buffer{}
	cmd := exec.Command("/usr/bin/script", args...)
	cmd.Env = append(os.Environ(), cliHelperEnv+"=1", "AMQ_NO_UPDATE_CHECK=1")
	cmd.Stdout = output
	cmd.Stderr = output
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create public coop PTY stdin: %v", err)
	}
	cmd.Stdin = stdinReader
	if err := cmd.Start(); err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		t.Fatalf("start public coop PTY: %v", err)
	}
	if err := stdinReader.Close(); err != nil {
		_ = stdinWriter.Close()
		_ = cmd.Process.Kill()
		t.Fatalf("close parent PTY stdin reader: %v", err)
	}
	fixture := &publicCoopPTYFixture{
		root:          root,
		ownerPath:     ownerPath,
		linePath:      linePath,
		interruptPath: interruptPath,
		cmd:           cmd,
		done:          make(chan error, 1),
		output:        output,
		stdin:         stdinWriter,
	}
	go func() { fixture.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if fixture.stdin != nil {
			_ = fixture.stdin.Close()
			fixture.stdin = nil
		}
		if !fixture.waited && fixture.cmd.Process != nil {
			if fixture.ownerPID > 1 {
				_ = syscall.Kill(fixture.ownerPID, syscall.SIGKILL)
			}
			_ = fixture.cmd.Process.Kill()
			select {
			case <-fixture.done:
			case <-time.After(3 * time.Second):
			}
		}
		if fixture.wakeClaim != nil {
			current := inspectWakeLock(fixture.root, "codex")
			if sameWakeLockGeneration(*fixture.wakeClaim, current) &&
				current.PID == fixture.wakeClaim.PID {
				_ = syscall.Kill(current.PID, syscall.SIGTERM)
			}
		}
	})
	return fixture
}

func waitForPublicCoopWakeReady(t *testing.T, fixture *publicCoopPTYFixture) wakeLockInspection {
	t.Helper()
	deadline := time.Now().Add(publicCoopPTYTimeout)
	for time.Now().Before(deadline) {
		inspection := inspectWakeLock(fixture.root, "codex")
		if confirmedLiveWake(inspection) {
			fixture.ownerPID = waitForPIDFile(t, fixture.ownerPath, publicCoopPTYTimeout)
			retained := inspection
			fixture.wakeClaim = &retained
			return inspection
		}
		select {
		case err := <-fixture.done:
			fixture.waited = true
			output := fixture.output.String()
			if strings.Contains(output, "TIOCSTI not available") ||
				strings.Contains(output, "amq wake requires a real terminal") {
				t.Skipf("public raw PTY runtime unavailable on this host: %v\n%s", err, output)
			}
			t.Fatalf("public coop exited before wake readiness: %v\n%s", err, output)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fixture.cmd.Process != nil {
		_ = fixture.cmd.Process.Kill()
	}
	t.Fatalf("public coop wake did not become ready within %s\n%s", publicCoopPTYTimeout, fixture.output.String())
	return wakeLockInspection{}
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

func assertPublicWakeCleaned(t *testing.T, root, me string, wakePID int) {
	t.Helper()
	deadline := time.Now().Add(publicCoopPTYTimeout)
	for time.Now().Before(deadline) {
		inspection := inspectWakeLock(root, me)
		process := inspectWakeProcess(wakePID)
		if !inspection.Exists && !process.Running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("public coop wake survived owner exit: lock=%#v process=%#v",
		inspectWakeLock(root, me), inspectWakeProcess(wakePID))
}

func waitForAdversarialPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("path %s did not appear within %s", path, timeout)
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastData []byte
	var lastErr error
	for time.Now().Before(deadline) {
		lastData, lastErr = os.ReadFile(path)
		if lastErr == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(lastData)))
			if parseErr == nil && pid > 1 {
				return pid
			}
			lastErr = parseErr
		} else if !os.IsNotExist(lastErr) {
			t.Fatalf("read PID file %s: %v", path, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID file %s did not contain a safe PID within %s: data=%q err=%v",
		path, timeout, lastData, lastErr)
	return 0
}
