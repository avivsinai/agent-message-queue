//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const wakeRepairLifecycleDeadPID = 424242

type wakeRepairLifecycleFixture struct {
	root       string
	target     wakeTarget
	lineage    wakeRepairLineage
	lockPath   string
	outputPath string
	agentDir   *wakeAgentDir
	inboxDir   *wakeInboxDir
}

func TestWakeRepairPreparedChildCannotNotifyBeforeAdmission(t *testing.T) {
	fixture := newWakeRepairLifecycleFixture(t)
	if err := os.Remove(fixture.lockPath); err != nil {
		t.Fatalf("remove dead source lock: %v", err)
	}

	child, err := startWakeFromTargetDefault(
		fixture.agentDir,
		fixture.inboxDir,
		fixture.root,
		"codex",
		fixture.target,
		fixture.lineage,
	)
	cleanupRepairLifecycleChild(t, child)
	if err != nil {
		t.Fatalf(
			"start prepared repair child: %v\n%s",
			err,
			wakeRepairLifecycleDiagnostics(fixture, child),
		)
	}

	if !processAlive(child.Process.Pid) {
		t.Fatal("prepared repair child exited before parent admission decision")
	}
	time.Sleep(250 * time.Millisecond)
	if output, readErr := os.ReadFile(fixture.outputPath); readErr == nil {
		t.Fatalf("prepared but unadmitted child injected notification: %q", output)
	} else if !os.IsNotExist(readErr) {
		t.Fatalf("read pre-admission injection output: %v", readErr)
	}

	agentDir, err := openWakeAgentDir(fixture.root, "codex")
	if err != nil {
		t.Fatalf("open retained repair directory: %v", err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := cleanupFailedWakeRepairChild(agentDir, fixture.root, "codex", child); err != nil {
		t.Fatalf("cleanup prepared but unadmitted child: %v", err)
	}
	assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
}

func TestWakeRepairChildUsesExactPersistedTargetAcrossCreationSecond(t *testing.T) {
	fixture := newWakeRepairLifecycleFixture(t)
	if err := os.Remove(fixture.lockPath); err != nil {
		t.Fatalf("remove dead source lock: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().UTC().Format(time.RFC3339) == fixture.target.Created {
		if time.Now().After(deadline) {
			t.Fatal("wall clock did not advance beyond persisted target creation second")
		}
		time.Sleep(10 * time.Millisecond)
	}

	child, err := startWakeFromTargetDefault(
		fixture.agentDir,
		fixture.inboxDir,
		fixture.root,
		"codex",
		fixture.target,
		fixture.lineage,
	)
	cleanupRepairLifecycleChild(t, child)
	if err != nil {
		t.Fatalf(
			"start repair child from exact persisted target: %v\n%s",
			err,
			wakeRepairLifecycleDiagnostics(fixture, child),
		)
	}
	if child.Prepared.ChildPID() != child.Process.Pid {
		t.Fatalf(
			"prepared child pid=%d, want exact process pid=%d",
			child.Prepared.ChildPID(),
			child.Process.Pid,
		)
	}
	if err := cleanupFailedWakeRepairChild(
		fixture.agentDir,
		fixture.root,
		"codex",
		child,
	); err != nil {
		t.Fatalf("cleanup prepared repair child: %v", err)
	}
	assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
}

func TestWakeRepairParentWinnerMismatchStopsAndReapsPreparedChild(t *testing.T) {
	fixture := newWakeRepairLifecycleFixture(t)
	var child *wakeRepairChild
	var startupDiagnostics string
	stubRealRepairStarter(
		t,
		func(started *wakeRepairChild, startErr error) {
			child = started
			if startErr != nil {
				startupDiagnostics = wakeRepairLifecycleDiagnostics(fixture, started)
			}
		},
		func(started *wakeRepairChild) {
			forceRepairLifecycleChildInspection(t, fixture, started)
			started.Prepared.childPID++
		},
	)

	result, err := repairWake(fixture.root, "codex")
	if err == nil || !strings.Contains(err.Error(), "does not match started pid") {
		t.Fatalf(
			"winner mismatch result=%#v err=%v\n%s",
			result,
			err,
			startupDiagnostics,
		)
	}
	if strings.Contains(err.Error(), "(cleanup:") {
		t.Fatalf("winner mismatch left child cleanup failure: %v", err)
	}
	assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
}

func TestWakeRepairAdmissionFailureStopsAndReapsPreparedChild(t *testing.T) {
	fixture := newWakeRepairLifecycleFixture(t)
	var child *wakeRepairChild
	var startupDiagnostics string
	stubRealRepairStarter(
		t,
		func(started *wakeRepairChild, startErr error) {
			child = started
			if startErr != nil {
				startupDiagnostics = wakeRepairLifecycleDiagnostics(fixture, started)
			}
		},
		func(started *wakeRepairChild) {
			forceRepairLifecycleChildInspection(t, fixture, started)
			started.admit = func() error {
				return errors.New("injected admission failure")
			}
		},
	)

	result, err := repairWake(fixture.root, "codex")
	if err == nil || !strings.Contains(err.Error(), "injected admission failure") {
		t.Fatalf(
			"admission failure result=%#v err=%v\n%s",
			result,
			err,
			startupDiagnostics,
		)
	}
	if strings.Contains(err.Error(), "(cleanup:") {
		t.Fatalf("admission failure left child cleanup failure: %v", err)
	}
	assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
}

func TestWakeRepairFailedChildCleanupPreservesFloorReplacedBeforeCleanup(t *testing.T) {
	tests := []struct {
		name   string
		mutate bool
	}{
		{name: "byte-identical new inode"},
		{name: "same generation and source digest changed bytes", mutate: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newWakeRepairLifecycleFixture(t)
			var child *wakeRepairChild
			var replacement []byte
			var startupDiagnostics string
			stubRealRepairStarter(
				t,
				func(started *wakeRepairChild, startErr error) {
					child = started
					if startErr != nil {
						startupDiagnostics = wakeRepairLifecycleDiagnostics(fixture, started)
					}
				},
				func(started *wakeRepairChild) {
					forceRepairLifecycleChildInspection(t, fixture, started)
					replacement = replaceWakeRepairFloorWithNewInodeForTest(
						t,
						fixture.root,
						"codex",
						tc.mutate,
					)
					started.Prepared.childPID++
				},
			)

			result, err := repairWake(fixture.root, "codex")
			if err == nil || !strings.Contains(err.Error(), "does not match started pid") {
				t.Fatalf(
					"winner mismatch result=%#v err=%v\n%s",
					result,
					err,
					startupDiagnostics,
				)
			}
			if strings.Contains(err.Error(), "(cleanup:") {
				t.Fatalf("replacement-preserving cleanup failed: %v", err)
			}
			assertRepairLifecycleChildReapedWithFloor(t, fixture, child, replacement)
		})
	}
}

func wakeRepairLifecycleDiagnostics(
	fixture wakeRepairLifecycleFixture,
	child *wakeRepairChild,
) string {
	var diagnostics strings.Builder
	fmt.Fprintf(
		&diagnostics,
		"wake-repair diagnostics root=%q agent-dir=%q inbox=%q\n",
		fixture.root,
		fsq.AgentBase(fixture.root, "codex"),
		filepath.Dir(fsq.AgentInboxNew(fixture.root, "codex")),
	)
	if child == nil {
		diagnostics.WriteString("child=<nil>\n")
	} else {
		fmt.Fprintf(
			&diagnostics,
			"child pid=%d alive=%v process-start=%q source=%+v prepared=%+v handoff=%t capability=%t waiter=%t\n",
			wakeRepairLifecycleChildPID(child),
			wakeRepairLifecycleChildAlive(child),
			child.ProcessStart,
			child.Source,
			child.Prepared,
			child.Handoff != nil,
			child.Capability != nil,
			child.Waiter != nil,
		)
	}

	inspection := inspectWakeLock(fixture.root, "codex")
	fmt.Fprintf(
		&diagnostics,
		"lock exists=%v status=%q identity-confirmed=%v reason=%q pid=%d control-socket=%q lock=%+v\n",
		inspection.Exists,
		inspection.Status,
		inspection.IdentityConfirmed,
		inspection.Reason,
		inspection.PID,
		inspection.Lock.ControlSocket,
		inspection.Lock,
	)

	agentBase := fsq.AgentBase(fixture.root, "codex")
	for _, name := range []string{
		".wake.lock",
		wakeTargetFileName,
		wakeRepairFloorFileName,
		wakePreparedFileName,
		".wake.repair.log",
	} {
		fmt.Fprintf(
			&diagnostics,
			"state %s\n",
			wakeRepairLifecycleFileDiagnostic(filepath.Join(agentBase, name)),
		)
	}

	pid := wakeRepairLifecycleChildPID(child)
	if pid <= 0 {
		return diagnostics.String()
	}
	for _, command := range [][]string{
		{"ps", "-p", strconv.Itoa(pid), "-o", "pid=,ppid=,state=,etime=,command="},
		{"lsof", "-nP", "-p", strconv.Itoa(pid)},
	} {
		fmt.Fprintf(
			&diagnostics,
			"command %q\n%s\n",
			strings.Join(command, " "),
			wakeRepairLifecycleCommandDiagnostic(command[0], command[1:]...),
		)
	}
	if runtime.GOOS == "darwin" {
		command := []string{"sample", strconv.Itoa(pid), "1", "1"}
		fmt.Fprintf(
			&diagnostics,
			"command %q\n%s\n",
			strings.Join(command, " "),
			wakeRepairLifecycleCommandDiagnostic(command[0], command[1:]...),
		)
	}
	fmt.Fprintf(
		&diagnostics,
		"go-stack %s\n",
		wakeRepairLifecycleGoStackDiagnostic(fixture, child),
	)
	return diagnostics.String()
}

func wakeRepairLifecycleChildPID(child *wakeRepairChild) int {
	if child == nil || child.Process == nil {
		return 0
	}
	return child.Process.Pid
}

func wakeRepairLifecycleChildAlive(child *wakeRepairChild) bool {
	pid := wakeRepairLifecycleChildPID(child)
	return pid > 0 && processAlive(pid)
}

func wakeRepairLifecycleFileDiagnostic(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Sprintf("path=%q lstat=%v", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf(
			"path=%q mode=%s size=%d data=<not-read: non-regular>",
			path,
			info.Mode(),
			info.Size(),
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf(
			"path=%q mode=%s size=%d open=%v",
			path,
			info.Mode(),
			info.Size(),
			err,
		)
	}
	defer func() { _ = file.Close() }()
	const diagnosticFileLimit = 16 << 10
	data, readErr := io.ReadAll(io.LimitReader(file, diagnosticFileLimit+1))
	if len(data) > diagnosticFileLimit {
		data = append(data[:diagnosticFileLimit], []byte("\n<truncated>")...)
	}
	return fmt.Sprintf(
		"path=%q mode=%s size=%d data=%q read=%v",
		path,
		info.Mode(),
		info.Size(),
		data,
		readErr,
	)
}

type wakeRepairLifecycleBoundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (buffer *wakeRepairLifecycleBoundedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		buffer.data = append(buffer.data, data[:remaining]...)
	}
	if remaining < len(data) {
		buffer.truncated = true
	}
	return len(data), nil
}

func (buffer *wakeRepairLifecycleBoundedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	output := string(buffer.data)
	if buffer.truncated {
		output += "\n<truncated>"
	}
	return output
}

func wakeRepairLifecycleCommandDiagnostic(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	const diagnosticCommandLimit = 32 << 10
	output := &wakeRepairLifecycleBoundedBuffer{limit: diagnosticCommandLimit}
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = output
	command.Stderr = output
	command.WaitDelay = 500 * time.Millisecond
	err := command.Run()
	return fmt.Sprintf("output=%q err=%v context=%v", output.String(), err, ctx.Err())
}

func wakeRepairLifecycleGoStackDiagnostic(
	fixture wakeRepairLifecycleFixture,
	child *wakeRepairChild,
) string {
	if child == nil || child.Process == nil || child.Process.Pid <= 0 {
		return "child unavailable"
	}
	if err := child.Process.Signal(syscall.SIGQUIT); err != nil {
		return fmt.Sprintf("signal SIGQUIT: %v", err)
	}
	if child.Waiter != nil {
		select {
		case <-child.Waiter.done:
		case <-time.After(2 * time.Second):
		}
	} else {
		time.Sleep(200 * time.Millisecond)
	}
	return wakeRepairLifecycleFileDiagnostic(
		filepath.Join(fsq.AgentBase(fixture.root, "codex"), ".wake.repair.log"),
	)
}

func newWakeRepairLifecycleFixture(t *testing.T) wakeRepairLifecycleFixture {
	t.Helper()
	t.Setenv(cliHelperEnv, "1")
	t.Setenv("AMQ_NO_UPDATE_CHECK", "1")

	root := wakeRepairLifecycleTempDir(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatalf("open lifecycle agent directory: %v", err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatalf("open lifecycle inbox directory: %v", err)
	}
	t.Cleanup(func() { _ = inboxDir.Close() })
	agentIdentity, err := wakeRepairDirectoryIdentityForFile(agentDir.file)
	if err != nil {
		t.Fatalf("identify lifecycle agent directory: %v", err)
	}
	inboxIdentity, err := wakeRepairDirectoryIdentityForFile(inboxDir.file)
	if err != nil {
		t.Fatalf("identify lifecycle inbox directory: %v", err)
	}

	outputPath := filepath.Join(root, "injected.txt")
	injector := filepath.Join(wakeRepairLifecycleTempDir(t), "injector")
	script := "#!/bin/sh\noutput=$1\nshift\nprintf '%s\\n' \"$@\" >> \"$output\"\n"
	if err := os.WriteFile(injector, []byte(script), 0o700); err != nil {
		t.Fatalf("write lifecycle injector: %v", err)
	}
	target, err := newWakeTarget(root, "codex", injector, []string{outputPath})
	if err != nil {
		t.Fatalf("new lifecycle target: %v", err)
	}

	bootID := inspectWakeProcessPlatform(os.Getpid()).BootID
	if bootID == "" {
		t.Fatal("current boot identity is unavailable")
	}
	targetDigest, err := wakeTargetDigest(target)
	if err != nil {
		t.Fatalf("digest lifecycle target: %v", err)
	}
	deadLock := wakeLock{
		PID:          wakeRepairLifecycleDeadPID,
		ProcessStart: "dead-process-start",
		BootID:       bootID,
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "dead-generation",
		WakeMode:     wakeTargetInjectVia,
		TargetDigest: targetDigest,
	}
	lockPath := writeWakeRepairLifecycleLock(t, root, "codex", deadLock)
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("write lifecycle target: %v", err)
	}
	floor, err := newWakeRepairFloor(root, "codex", deadLock, target, nil)
	if err != nil {
		t.Fatalf("new lifecycle floor: %v", err)
	}
	if err := writeWakeRepairFloor(root, "codex", floor); err != nil {
		t.Fatalf("write lifecycle floor: %v", err)
	}
	floorDigest, err := wakeRepairFloorDigest(floor)
	if err != nil {
		t.Fatalf("digest lifecycle floor: %v", err)
	}
	lineage := wakeRepairLineage{
		source: wakeRepairSource{
			Root:               floor.Root,
			RootIdentity:       floor.RootIdentity,
			Agent:              floor.Agent,
			DeadGeneration:     floor.Generation,
			BootID:             floor.BootID,
			Owner:              floor.Owner,
			SourceTargetDigest: targetDigest,
			SourceFloorDigest:  floorDigest,
			AgentDirDevice:     agentIdentity.device,
			AgentDirInode:      agentIdentity.inode,
			InboxDirDevice:     inboxIdentity.device,
			InboxDirInode:      inboxIdentity.inode,
		},
		floor: floor,
	}
	writeWakeRepairLifecycleMessage(t, root)

	oldInspect := inspectWakeProcess
	inspectWakeProcess = func(pid int) wakeProcessInfo {
		if pid == wakeRepairLifecycleDeadPID {
			return wakeProcessInfo{PID: pid, Running: false}
		}
		return inspectWakeProcessPlatform(pid)
	}
	t.Cleanup(func() {
		inspectWakeProcess = oldInspect
	})

	return wakeRepairLifecycleFixture{
		root:       root,
		target:     target,
		lineage:    lineage,
		lockPath:   lockPath,
		outputPath: outputPath,
		agentDir:   agentDir,
		inboxDir:   inboxDir,
	}
}

func wakeRepairLifecycleTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "amq-wr-")
	if err != nil {
		t.Fatalf("create short wake repair lifecycle temp directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove wake repair lifecycle temp directory: %v", err)
		}
	})
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

func writeWakeRepairLifecycleLock(
	t *testing.T,
	root, me string,
	lock wakeLock,
) string {
	t.Helper()
	lock.Root = canonicalWakeRoot(root)
	lock.Agent = me
	lock.Started = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("marshal lifecycle lock: %v", err)
	}
	path := filepath.Join(fsq.AgentBase(root, me), ".wake.lock")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write lifecycle lock: %v", err)
	}
	return path
}

func writeWakeRepairLifecycleMessage(t *testing.T, root string) {
	t.Helper()
	message := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       "pending",
			From:     "claude",
			To:       []string{"codex"},
			Thread:   "p2p/claude__codex",
			Subject:  "must wait for admission",
			Created:  "2026-07-24T00:00:00Z",
			Priority: "normal",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatalf("marshal lifecycle message: %v", err)
	}
	path := filepath.Join(fsq.AgentInboxNew(root, "codex"), "pending.md")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write lifecycle message: %v", err)
	}
}

func stubRealRepairStarter(
	t *testing.T,
	capture func(*wakeRepairChild, error),
	mutate func(*wakeRepairChild),
) {
	t.Helper()
	old := startWakeFromTarget
	startWakeFromTarget = func(
		agentDir *wakeAgentDir,
		inboxDir *wakeInboxDir,
		root, me string,
		target wakeTarget,
		lineage wakeRepairLineage,
	) (*wakeRepairChild, error) {
		child, err := startWakeFromTargetDefault(
			agentDir,
			inboxDir,
			root,
			me,
			target,
			lineage,
		)
		if capture != nil {
			capture(child, err)
		}
		if err == nil {
			mutate(child)
		}
		return child, err
	}
	t.Cleanup(func() {
		startWakeFromTarget = old
	})
}

func cleanupRepairLifecycleChild(t *testing.T, child *wakeRepairChild) {
	t.Helper()
	t.Cleanup(func() {
		if child == nil || child.Process == nil || child.Process.Pid <= 0 {
			return
		}
		if processAlive(child.Process.Pid) {
			_ = child.Process.Kill()
		}
		if child.Handoff != nil {
			_ = child.Handoff.Close()
		}
		if child.Capability != nil {
			_ = child.Capability.Close()
		}
		if child.Waiter != nil {
			_ = child.Waiter.waitForExit(5 * time.Second)
		}
	})
}

func forceRepairLifecycleChildInspection(
	t *testing.T,
	fixture wakeRepairLifecycleFixture,
	child *wakeRepairChild,
) {
	t.Helper()
	if child == nil || child.Process == nil {
		t.Fatal("prepared repair child is missing")
	}
	inspection := inspectWakeLock(fixture.root, "codex")
	if !inspection.Exists {
		t.Fatal("prepared repair child did not publish a lock")
	}
	lock := inspection.Lock
	lock.Executable = "/opt/homebrew/bin/amq"
	lock.Args = []string{"amq", "wake", "--root", fixture.root, "--me", "codex"}
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("marshal authoritative child inspection fixture: %v", err)
	}
	if err := os.WriteFile(inspection.LockPath, data, 0o600); err != nil {
		t.Fatalf("write authoritative child inspection fixture: %v", err)
	}
	previous := inspectWakeProcess
	inspectWakeProcess = func(pid int) wakeProcessInfo {
		if pid == child.Process.Pid {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: lock.ProcessStart,
				BootID:     lock.BootID,
				Executable: lock.Executable,
				Args:       append([]string(nil), lock.Args...),
			}
		}
		return previous(pid)
	}
	t.Cleanup(func() {
		inspectWakeProcess = previous
	})
	confirmed := inspectWakeLock(fixture.root, "codex")
	if confirmed.Status != wakeLockValid || !confirmed.IdentityConfirmed {
		t.Fatalf(
			"forced prepared child inspection status=%q identity=%v reason=%q",
			confirmed.Status,
			confirmed.IdentityConfirmed,
			confirmed.Reason,
		)
	}
}

func assertRepairLifecycleChildReapedWithoutClaim(
	t *testing.T,
	fixture wakeRepairLifecycleFixture,
	child *wakeRepairChild,
) {
	t.Helper()
	if child == nil || child.Process == nil || child.Waiter == nil {
		t.Fatal("repair child did not retain exact process and waiter")
	}
	if err := child.Waiter.waitForExit(time.Second); err != nil {
		t.Fatalf("repair child was not reaped: %v", err)
	}
	if child.Waiter.state == nil {
		t.Fatal("repair child waiter completed without a process state")
	}
	if processAlive(child.Process.Pid) {
		t.Fatalf("repair child pid %d remains alive", child.Process.Pid)
	}
	if _, err := os.Lstat(fixture.lockPath); !os.IsNotExist(err) {
		t.Fatalf("repair child lock residue remains: %v", err)
	}
	if _, err := os.Lstat(wakeRepairFloorPath(fixture.root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("repair child floor residue remains: %v", err)
	}
}

func assertRepairLifecycleChildReapedWithFloor(
	t *testing.T,
	fixture wakeRepairLifecycleFixture,
	child *wakeRepairChild,
	expectedFloor []byte,
) {
	t.Helper()
	if child == nil || child.Process == nil || child.Waiter == nil {
		t.Fatal("repair child did not retain exact process and waiter")
	}
	if err := child.Waiter.waitForExit(time.Second); err != nil {
		t.Fatalf("repair child was not reaped: %v", err)
	}
	if child.Waiter.state == nil {
		t.Fatal("repair child waiter completed without a process state")
	}
	if processAlive(child.Process.Pid) {
		t.Fatalf("repair child pid %d remains alive", child.Process.Pid)
	}
	if _, err := os.Lstat(fixture.lockPath); !os.IsNotExist(err) {
		t.Fatalf("repair child lock residue remains: %v", err)
	}
	got, err := os.ReadFile(wakeRepairFloorPath(fixture.root, "codex"))
	if err != nil {
		t.Fatalf("replacement repair child floor was removed: %v", err)
	}
	if !bytes.Equal(got, expectedFloor) {
		t.Fatal("replacement repair child floor changed during cleanup")
	}
}
