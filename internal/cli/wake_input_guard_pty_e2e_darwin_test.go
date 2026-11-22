//go:build darwin

package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	wakeInputGuardPTYLiveEnv = "AMQ_WAKE_INPUT_GUARD_PTY_LIVE"
	wakeInputGuardEditorEnv  = "AMQ_TEST_WAKE_INPUT_GUARD_EDITOR"
	wakeInputGuardEditorLog  = "AMQ_E2E_EDITOR_LOG"
)

// TestWakeInputGuardEditor is the child used as the real wake's terminal
// reader. It consumes readable data, polls once more to EAGAIN as a TUI does,
// then preserves any partial prompt in the log. No vendor process or login is
// involved. It is intentionally opt-in and is never driven in an ordinary run.
func TestWakeInputGuardEditor(t *testing.T) {
	if os.Getenv(wakeInputGuardEditorEnv) == "" {
		t.Skip("external PTY input-guard editor helper")
	}
	fd := int(os.Stdin.Fd())
	if _, err := term.MakeRaw(fd); err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	logPath := os.Getenv(wakeInputGuardEditorLog)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(filepath.Dir(logPath), "stop")
	write := func(rec wakeInputGuardEditorRecord) {
		rec.At = time.Now().UTC()
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write(append(data, '\n')); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		_ = file.Close()
	}
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(terminated)
	write(wakeInputGuardEditorRecord{Kind: "ready"})
	var line []byte
	escape := byte(0)
	for {
		select {
		case <-terminated:
			write(wakeInputGuardEditorRecord{Kind: "terminated", Buffer: string(line)})
			return
		default:
		}
		if _, err := os.Stat(stop); err == nil {
			write(wakeInputGuardEditorRecord{Kind: "stop", Buffer: string(line)})
			return
		}
		ready, err := wakeInputGuardPTYReadable(fd, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !ready {
			continue
		}
		for {
			var chunk [512]byte
			n, err := os.Stdin.Read(chunk[:])
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EAGAIN) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range chunk[:n] {
				if b == 0x1b {
					escape = 1
					continue
				}
				switch escape {
				case 1:
					switch b {
					case '[':
						escape = 2
					case 'O':
						escape = 3
					default:
						escape = 0
					}
					continue
				case 2:
					// CSI parameters may span reads. Until the final byte,
					// neither digits nor semicolons are editor contents.
					if b >= 0x40 && b <= 0x7e {
						escape = 0
					}
					continue
				case 3:
					// SS3/focus reports have one final byte after ESC O.
					escape = 0
					continue
				}
				switch {
				case b == '\r':
					// A raw wake may retry Enter after a successful submit.
					// As in a TUI with no active prompt, an empty buffer
					// yields no submit event; the raw CR stays in this read log.
					if len(line) > 0 {
						write(wakeInputGuardEditorRecord{Kind: "submit", Text: string(line)})
						line = nil
					}
				case b == '\n':
					line = append(line, b)
				case b == 0x7f || b == 0x08:
					if len(line) > 0 {
						line = line[:len(line)-1]
					}
				case b >= 0x20 && b != 0x7f:
					line = append(line, b)
				}
			}
			write(wakeInputGuardEditorRecord{Kind: "read", Hex: hex.EncodeToString(chunk[:n]), Buffer: string(line)})
		}
	}
}

func wakeInputGuardPTYReadable(fd int, timeout time.Duration) (bool, error) {
	var readSet unix.FdSet
	readSet.Bits[fd/64] |= 1 << (uint(fd) % 64)
	tv := unix.NsecToTimeval(timeout.Nanoseconds())
	n, err := unix.Select(fd+1, &readSet, nil, nil, &tv)
	if errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return n > 0, err
}

type wakeInputGuardEditorRecord struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text,omitempty"`
	Hex    string    `json:"hex,omitempty"`
	Buffer string    `json:"buffer,omitempty"`
}

type wakeInputGuardPTYOutput struct {
	mu  sync.Mutex
	buf []byte
}

func (out *wakeInputGuardPTYOutput) Write(p []byte) (int, error) {
	out.mu.Lock()
	defer out.mu.Unlock()
	const limit = 16384
	out.buf = append(out.buf, p...)
	if len(out.buf) > limit {
		out.buf = append(out.buf[:0], out.buf[len(out.buf)-limit:]...)
	}
	return len(p), nil
}

func (out *wakeInputGuardPTYOutput) String() string {
	out.mu.Lock()
	defer out.mu.Unlock()
	return string(out.buf)
}

type wakeInputGuardPTYSeat struct {
	t      *testing.T
	binary string
	root   string
	log    string
	out    *wakeInputGuardPTYOutput
	master *os.File
	cmd    *exec.Cmd
	done   chan error
	mode   string
	wakeID int
}

func newWakeInputGuardPTYSeat(t *testing.T, mode string, enabled bool) *wakeInputGuardPTYSeat {
	return newWakeInputGuardPTYSeatAttended(t, mode, enabled, false)
}

// newWakeInputGuardPTYSeatAttended turns attended on for the seat when
// attended is true, through the same wake config the guard flag uses.
func newWakeInputGuardPTYSeatAttended(t *testing.T, mode string, enabled, attended bool) *wakeInputGuardPTYSeat {
	t.Helper()
	temp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(temp, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(temp, "queue")
	s := &wakeInputGuardPTYSeat{t: t, root: root, log: filepath.Join(temp, "editor.jsonl"), out: new(wakeInputGuardPTYOutput), mode: mode}
	s.binary = filepath.Join(temp, "amq")
	buildVersionedWakeRestartBinary(t, wakeSelfUpgradeE2ERepoRoot(t), s.binary, "input-guard-pty-test")
	work := wakeSelfUpgradeWorkDir(root)
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.run("init", "--root", root, "--agents", "seat,sender")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.cmd = exec.Command(s.binary, "coop", "exec", "--root", root, "--me", "seat", "--no-init", "--require-wake", "--wake-inject-mode", mode, testBinary, "-test.run=^TestWakeInputGuardEditor$")
	s.cmd.Dir = work
	s.cmd.Env = wakeABICleanEnv("HOME="+filepath.Join(temp, "home"),
		wakeInputGuardEditorEnv+"=1", wakeInputGuardEditorLog+"="+s.log, "AMQ_WAKE_NO_SELF_UPGRADE=1")
	s.cmd.Stdout = s.out
	s.cmd.Stderr = s.out
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	s.master, err = pty.Start(s.cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.done = make(chan error, 1)
	go func() { s.done <- s.cmd.Wait() }()
	t.Cleanup(s.close)
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		lock := inspectWakeLock(root, "seat")
		if lock.Exists && lock.PID > 0 && lock.Status == wakeLockValid && lock.IdentityConfirmed && lock.Process.Running {
			s.wakeID = lock.PID
			goto ready
		}
		s.sleep(25 * time.Millisecond)
	}
	s.fail("wake did not become ready")
ready:
	if attended {
		config := s.run("wake", "config", "--root", root, "--me", "seat", "--attended=true", "--wait")
		t.Logf("wake configuration after --wait: %s", config)
	}
	if enabled {
		config := s.run("wake", "config", "--root", root, "--me", "seat", "--input-guard=true", "--input-stale-for=12s", "--wait")
		t.Logf("wake configuration after --wait: %s", config)
	}
	if !s.waitUntil(5*time.Second, func(records []wakeInputGuardEditorRecord) bool {
		return len(records) > 0 && records[len(records)-1].Kind == "ready"
	}) {
		s.fail("editor did not become ready")
	}
	return s
}

func (s *wakeInputGuardPTYSeat) run(args ...string) string {
	s.t.Helper()
	args = append([]string{s.binary}, args...)
	output := exec.Command(args[0], args[1:]...)
	output.Dir = wakeSelfUpgradeWorkDir(s.root)
	output.Env = wakeABICleanEnv("HOME=" + filepath.Join(filepath.Dir(s.root), "home"))
	data, err := output.CombinedOutput()
	if err != nil {
		s.fail(fmt.Sprintf("command %v failed: %v", args, err))
	}
	return string(data)
}

func (s *wakeInputGuardPTYSeat) records() []wakeInputGuardEditorRecord {
	data, err := os.ReadFile(s.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.t.Errorf("read editor log: %v", err)
		return nil
	}
	var records []wakeInputGuardEditorRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record wakeInputGuardEditorRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			s.t.Errorf("invalid editor log line %q: %v", line, err)
			continue
		}
		records = append(records, record)
	}
	return records
}

func (s *wakeInputGuardPTYSeat) sleep(wait time.Duration) {
	tracer := time.NewTimer(wait)
	select {
	case <-tracer.C:
	case err := <-s.done:
		s.fail(fmt.Sprintf("coop exec exited before expected: %v", err))
	}
}

func (s *wakeInputGuardPTYSeat) waitUntil(timeout time.Duration, predicate func([]wakeInputGuardEditorRecord) bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate(s.records()) {
			return true
		}
		s.sleep(20 * time.Millisecond)
	}
	return false
}

func (s *wakeInputGuardPTYSeat) submit(timeout time.Duration, count int) wakeInputGuardEditorRecord {
	var found wakeInputGuardEditorRecord
	if !s.waitUntil(timeout, func(records []wakeInputGuardEditorRecord) bool {
		var submits []wakeInputGuardEditorRecord
		for _, record := range records {
			if record.Kind == "submit" {
				submits = append(submits, record)
			}
		}
		if len(submits) > count {
			found = submits[count]
			return true
		}
		return false
	}) {
		s.fail(fmt.Sprintf("missing submit %d after %s", count, timeout))
	}
	return found
}

func (s *wakeInputGuardPTYSeat) sendAndSubmit(timeout time.Duration, count int) wakeInputGuardEditorRecord {
	before := time.Now()
	s.run("send", "--root", s.root, "--me", "sender", "--to", "seat", "--subject", "pty-case", "--body", "private PTY test; no model required")
	record := s.submit(timeout, count)
	if elapsed := record.At.Sub(before); elapsed < 0 || elapsed > timeout {
		s.fail(fmt.Sprintf("submitted doorbell at %s after send at %s (elapsed %s), want a new submission within %s", record.At, before, elapsed, timeout))
	}
	if record.Text != harness.Doorbell {
		s.fail(fmt.Sprintf("submitted text %q, want the exact doorbell without control sequences", record.Text))
	}
	return record
}

func (s *wakeInputGuardPTYSeat) deliveryFinished(timeout time.Duration) {
	var ledgerErr error
	finished := s.waitUntil(timeout, func([]wakeInputGuardEditorRecord) bool {
		attempts, err := notificationattempt.List(s.root, "seat", "")
		if err != nil {
			ledgerErr = err
			return false
		}
		for _, attempt := range attempts {
			if attempt.Result != nil && attempt.Result.Phase == notificationattempt.PhaseResult {
				finishedAt, err := time.Parse(time.RFC3339Nano, attempt.Result.RecordedAt)
				if err != nil {
					ledgerErr = err
					return false
				}
				// The result follows raw injection, rescue Enter and the input
				// sample. Only later can the typed text test its REMINDER.
				return time.Since(finishedAt) >= rawInjectDrainPollInterval
			}
		}
		return false
	})
	if !finished {
		s.fail(fmt.Sprintf("first notification did not record a finished result (ledger error %v)", ledgerErr))
	}
}

func (s *wakeInputGuardPTYSeat) drain() {
	s.run("drain", "--root", s.root, "--me", "seat")
}

func (s *wakeInputGuardPTYSeat) typeInput(text string) {
	if _, err := io.WriteString(s.master, text); err != nil {
		s.fail(fmt.Sprintf("write PTY input: %v", err))
	}
}

func (s *wakeInputGuardPTYSeat) requireDuring(wait time.Duration, predicate func([]wakeInputGuardEditorRecord) bool, label string) {
	until := time.Now().Add(wait)
	for time.Now().Before(until) {
		if !predicate(s.records()) {
			s.fail(label)
		}
		s.sleep(20 * time.Millisecond)
	}
	if !predicate(s.records()) {
		s.fail(label)
	}
}

func (s *wakeInputGuardPTYSeat) close() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	}
	if s.master != nil {
		_ = s.master.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			_ = s.cmd.Process.Kill()
			select {
			case <-s.done:
			case <-time.After(2 * time.Second):
				s.t.Errorf("PTY editor did not finish after SIGKILL")
			}
		}
	}
	reapSpawnedWakeSelfUpgradePID(s.t, s.wakeID)
}

func (s *wakeInputGuardPTYSeat) fail(reason string) {
	diagnosticCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	check := exec.CommandContext(diagnosticCtx, s.binary, "wake", "check", "--root", s.root, "--me", "seat", "--json")
	check.Dir = wakeSelfUpgradeWorkDir(s.root)
	check.Env = wakeABICleanEnv("HOME=" + filepath.Join(filepath.Dir(s.root), "home"))
	diagnostic, _ := check.CombinedOutput()
	s.t.Fatalf("%s\neditor: %#v\nexec output: %s\nwake check (%v): %s", reason, s.records(), s.out.String(), diagnosticCtx.Err(), diagnostic)
}

// TestWakeInputGuardRealPTY catches the timestamp boundary replaced by every
// function-hook unit test: an idle reader still changes atime. It must not use
// any real terminal, vendor harness, home configuration or waking seat.
func TestWakeInputGuardRealPTY(t *testing.T) {
	if os.Getenv(wakeInputGuardPTYLiveEnv) != "1" {
		t.Skip(wakeInputGuardPTYLiveEnv + "=1 is required; this proof creates real ptys and local wake processes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 171*time.Second)
	defer cancel()
	t.Cleanup(func() {
		if ctx.Err() == context.DeadlineExceeded {
			t.Errorf("real PTY proof exceeded its three-minute bound: %v", ctx.Err())
		}
	})
	t.Run("guard off rings as before", func(t *testing.T) {
		s := newWakeInputGuardPTYSeat(t, wakeInjectModeRaw, false)
		if !strings.Contains(s.out.String(), "input guard: off") {
			t.Fatalf("coop exec output = %q, want input guard: off", s.out.String())
		}
		for i := 0; i < 3; i++ {
			s.sendAndSubmit(4*time.Second, i)
			s.drain()
		}
	})
	for _, mode := range []string{wakeInjectModeRaw, wakeInjectModePaste} {
		t.Run("guard on rings at once after own write "+mode, func(t *testing.T) {
			s := newWakeInputGuardPTYSeat(t, mode, true)
			for i := 0; i < 5; i++ {
				s.sendAndSubmit(2*time.Second, i)
				s.drain()
			}
		})
	}
	t.Run("guard on leaves a draft alone", func(t *testing.T) {
		const draft = "draft"
		s := newWakeInputGuardPTYSeat(t, wakeInjectModeRaw, true)
		// A fresh, unhooked seat uses the quiet window until its first own
		// write. Establish that state before testing the stale-input hold.
		s.sendAndSubmit(2*time.Second, 0)
		s.deliveryFinished(2 * time.Second)
		s.drain()
		priorSubmits := 0
		for _, record := range s.records() {
			if record.Kind == "submit" {
				priorSubmits++
			}
		}
		s.typeInput("draft")
		if !s.waitUntil(2*time.Second, func(records []wakeInputGuardEditorRecord) bool {
			return len(records) > 0 && records[len(records)-1].Buffer == "draft"
		}) {
			t.Fatal("editor did not read the human draft")
		}
		s.requireDuring(3*time.Second, func(records []wakeInputGuardEditorRecord) bool {
			return len(records) > 0 && records[len(records)-1].Buffer == "draft"
		}, "editor did not retain draft before mail")
		sent := time.Now()
		s.run("send", "--root", s.root, "--me", "sender", "--to", "seat", "--body", "draft test")
		s.requireDuring(8*time.Second, func(records []wakeInputGuardEditorRecord) bool {
			seen := 0
			for _, record := range records {
				if record.Kind != "submit" {
					continue
				}
				seen++
				if seen > priorSubmits {
					return false
				}
			}
			return records[len(records)-1].Buffer == "draft"
		}, "a submit replaced draft while input was recent")
		record := s.submit(30*time.Second, priorSubmits)
		if elapsed := record.At.Sub(sent); elapsed < 8*time.Second || elapsed > 30*time.Second {
			s.fail(fmt.Sprintf("stale doorbell took %s after this mail, want [8s,30s]", elapsed))
		}
		if record.Text != draft+harness.Doorbell {
			s.fail(fmt.Sprintf("stale submission = %q, want preserved draft plus the exact full doorbell", record.Text))
		}
		seen := 0
		for _, submitted := range s.records() {
			if submitted.Kind != "submit" {
				continue
			}
			seen++
			if seen > priorSubmits && submitted.At.After(sent) && submitted.Text == draft {
				s.fail("a lone Enter after the hold submitted the draft alone")
			}
		}
		if seen != priorSubmits+1 {
			s.fail(fmt.Sprintf("post-draft submissions = %d, want exactly one eventual stale doorbell", seen-priorSubmits))
		}
	})
	t.Run("guard on never sends a lone Enter into typed text", func(t *testing.T) {
		s := newWakeInputGuardPTYSeat(t, wakeInjectModeRaw, true)
		s.sendAndSubmit(2*time.Second, 0)
		s.deliveryFinished(2 * time.Second)
		// The earlier initial-delivery collision window is a separate issue.
		// This case tests the later cohort reminder, after raw delivery has
		// completed. Never treat its first mail as drained; none was read.
		s.typeInput("xyz")
		if !s.waitUntil(2*time.Second, func(records []wakeInputGuardEditorRecord) bool {
			return len(records) > 0 && records[len(records)-1].Buffer == "xyz"
		}) {
			t.Fatal("editor did not read typed text after the first doorbell")
		}
		s.requireDuring(10*time.Second, func(records []wakeInputGuardEditorRecord) bool {
			return len(records) > 0 && records[len(records)-1].Buffer == "xyz"
		}, "Typed text lost its editor buffer, or a bare Enter submitted it")
		record := s.submit(30*time.Second, 1)
		if !strings.Contains(record.Text, harness.Doorbell) {
			t.Fatalf("second record = %q, want the full doorbell instead of a typed-string submit", record.Text)
		}
	})
	t.Run("guard on survives a focus report", func(t *testing.T) {
		s := newWakeInputGuardPTYSeat(t, wakeInjectModeRaw, true)
		s.sendAndSubmit(2*time.Second, 0)
		s.deliveryFinished(2 * time.Second)
		s.drain()
		typeInputIndex := len(s.records())
		for _, input := range []struct {
			text string
			hint string
		}{{"\x1b[O", "focus-out"}, {"\x1b[I", "focus-in"}} {
			prior := len(s.records())
			s.typeInput(input.text)
			if !s.waitUntil(2*time.Second, func(records []wakeInputGuardEditorRecord) bool {
				if len(records) <= prior || records[len(records)-1].Buffer != "" {
					return false
				}
				var raw []byte
				for _, record := range records[prior:] {
					chunk, err := hex.DecodeString(record.Hex)
					if err != nil {
						return false
					}
					raw = append(raw, chunk...)
				}
				return strings.Contains(string(raw), input.text)
			}) {
				s.fail(input.hint + " was not read across its real chunks with an empty editor buffer")
			}
		}
		for _, record := range s.records()[typeInputIndex:] {
			if record.Buffer != "" {
				s.fail("a focus report left text in the editor buffer")
			}
		}
		s.sendAndSubmit(30*time.Second, 1)
		s.drain()
		s.sendAndSubmit(2*time.Second, 2)
	})
	// The end-to-end proof of the attended invariant: with a
	// draft in the editor, three mails five seconds apart type no byte and
	// submit nothing. The wake's attention output is the only face the mail
	// gets; the model learns of it at the person's submit.
	t.Run("attended seat is never typed into", func(t *testing.T) {
		s := newWakeInputGuardPTYSeatAttended(t, wakeInjectModeRaw, false, true)
		s.typeInput("draft")
		if !s.waitUntil(2*time.Second, func(records []wakeInputGuardEditorRecord) bool {
			return len(records) > 0 && records[len(records)-1].Buffer == "draft"
		}) {
			t.Fatal("editor did not read the human draft")
		}
		sent := time.Now()
		for i := 0; i < 3; i++ {
			s.run("send", "--root", s.root, "--me", "sender", "--to", "seat", "--subject", "attended-case", "--body", "attended PTY test; no model required")
			s.sleep(5 * time.Second)
		}
		s.requireDuring(20*time.Second, func(records []wakeInputGuardEditorRecord) bool {
			for _, record := range records {
				if record.Kind == "submit" && record.At.After(sent) {
					s.fail("attended seat submitted the draft or typed the doorbell")
					return false
				}
			}
			return records[len(records)-1].Buffer == "draft"
		}, "the editor lost its draft, or the wake typed input on an attended seat")
		// The person types nothing after the draft, so any read record after
		// it is a wake byte, even one the editor discards (BEL, an escape
		// sequence, 0x03): the byte log, not the buffer, is the proof.
		s.requireDuring(1*time.Second, func(records []wakeInputGuardEditorRecord) bool {
			for _, record := range records {
				if record.Kind == "read" && record.At.After(sent) {
					s.fail("attended seat wrote a byte after the draft: " + record.Hex)
					return false
				}
			}
			return true
		}, "the wake wrote to the attended seat's terminal after the draft")
	})

}
