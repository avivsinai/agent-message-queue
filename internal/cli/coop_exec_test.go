//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSplitDashDash(t *testing.T) {
	tests := []struct {
		name       string
		input      []string
		wantBefore []string
		wantAfter  []string
	}{
		{
			name:       "no separator",
			input:      []string{"claude"},
			wantBefore: []string{"claude"},
			wantAfter:  nil,
		},
		{
			name:       "separator with args",
			input:      []string{"--root", "/tmp/q", "codex", "--", "--some-flag", "--other"},
			wantBefore: []string{"--root", "/tmp/q", "codex"},
			wantAfter:  []string{"--some-flag", "--other"},
		},
		{
			name:       "separator at start",
			input:      []string{"--", "claude", "-v"},
			wantBefore: []string{},
			wantAfter:  []string{"claude", "-v"},
		},
		{
			name:       "separator at end",
			input:      []string{"claude", "--"},
			wantBefore: []string{"claude"},
			wantAfter:  []string{},
		},
		{
			name:       "empty input",
			input:      []string{},
			wantBefore: []string{},
			wantAfter:  nil,
		},
		{
			name:       "multiple separators",
			input:      []string{"a", "--", "b", "--", "c"},
			wantBefore: []string{"a"},
			wantAfter:  []string{"b", "--", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := splitDashDash(tt.input)
			if !sliceEq(before, tt.wantBefore) {
				t.Errorf("before = %v, want %v", before, tt.wantBefore)
			}
			if !sliceEq(after, tt.wantAfter) {
				t.Errorf("after = %v, want %v", after, tt.wantAfter)
			}
		})
	}
}

func TestSetEnvVar(t *testing.T) {
	t.Run("append new", func(t *testing.T) {
		env := []string{"PATH=/bin", "HOME=/home"}
		got := setEnvVar(env, "AM_ROOT", "/tmp/q")
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[2] != "AM_ROOT=/tmp/q" {
			t.Fatalf("got[2] = %q, want %q", got[2], "AM_ROOT=/tmp/q")
		}
	})

	t.Run("replace existing", func(t *testing.T) {
		env := []string{"PATH=/bin", "AM_ROOT=/old", "HOME=/home"}
		got := setEnvVar(env, "AM_ROOT", "/new")
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[1] != "AM_ROOT=/new" {
			t.Fatalf("got[1] = %q, want %q", got[1], "AM_ROOT=/new")
		}
	})
}

func TestCoopExecUsageError(t *testing.T) {
	err := runCoopExec([]string{})
	if err == nil {
		t.Fatal("expected error for empty args")
	}
	exitErr, ok := err.(*ExitCodeError)
	if !ok {
		t.Fatalf("expected *ExitCodeError, got %T: %v", err, err)
	}
	if exitErr.Code != ExitUsage {
		t.Fatalf("expected ExitUsage (%d), got %d", ExitUsage, exitErr.Code)
	}
	if !containsStr(err.Error(), "command required") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCoopExecForwardsCommandArguments(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("exec sentinel")
	var gotArgv []string
	oldExec := coopExecProcess
	coopExecProcess = func(_ string, argv []string, _ []string) error {
		gotArgv = append([]string{}, argv...)
		return sentinel
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	err := runCoopExec([]string{
		"--root", root,
		"--me", "codex",
		"--no-wake",
		"sh", "-c", "echo ok",
		"--", "--tail-flag",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want sentinel", err)
	}
	wantArgv := []string{"sh", "-c", "echo ok", "--tail-flag"}
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", gotArgv, wantArgv)
	}
}

func TestCoopExecSessionRootMutuallyExclusive(t *testing.T) {
	err := runCoopExec([]string{"--session", "feat", "--root", "/tmp/q", "claude"})
	if err == nil {
		t.Fatal("expected error for --session + --root")
	}
	if !containsStr(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCoopExecRequireWakeRejectsNoWake(t *testing.T) {
	err := runCoopExec([]string{"--require-wake", "--no-wake", "claude"})
	if err == nil {
		t.Fatal("expected error for --require-wake + --no-wake")
	}
	if !containsStr(err.Error(), "--require-wake cannot be used with --no-wake") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCoopExecAlwaysOverwritesOwnerTokenImmediatelyBeforeExec(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	owner := wakeOwner{
		PID:          self,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != self {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: owner.ProcessStart,
			BootID:     owner.BootID,
		}
	})
	stubWakeProcessSID(t, func(pid int) (int, error) {
		if pid != self {
			t.Fatalf("session lookup pid = %d, want %d", pid, self)
		}
		return owner.SessionID, nil
	})
	t.Setenv(envWakeOwner, `{"pid":1,"process_start":"stale","boot_id":"stale","session_id":1}`)

	sentinel := errors.New("exec sentinel")
	var execEnv []string
	oldExec := coopExecProcess
	coopExecProcess = func(_ string, _ []string, env []string) error {
		execEnv = append([]string{}, env...)
		return sentinel
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	err := runCoopExec([]string{"--root", root, "--me", "codex", "--no-wake", "sh"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want sentinel", err)
	}
	raw := ""
	count := 0
	for _, entry := range execEnv {
		if strings.HasPrefix(entry, envWakeOwner+"=") {
			count++
			raw = strings.TrimPrefix(entry, envWakeOwner+"=")
		}
	}
	if count != 1 {
		t.Fatalf("final %s count = %d, env=%v", envWakeOwner, count, execEnv)
	}
	var got wakeOwner
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode final owner: %v", err)
	}
	if got != owner {
		t.Fatalf("final owner = %#v, want %#v", got, owner)
	}
}

func TestCoopExecRejectsExecReturningNil(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	oldExec := coopExecProcess
	coopExecProcess = func(string, []string, []string) error { return nil }
	t.Cleanup(func() { coopExecProcess = oldExec })

	err := runCoopExec([]string{"--root", root, "--me", "codex", "--no-wake", "sh"})
	if err == nil || !strings.Contains(err.Error(), "exec returned without replacing process") {
		t.Fatalf("coop exec error = %v, want nil-return refusal", err)
	}
}

func TestCoopExecRejectsNilWakeChildCapability(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	oldPrepare := prepareAuthoritativeWakeChild
	prepareAuthoritativeWakeChild = func(*exec.Cmd) (*authoritativeWakeChildCapability, error) {
		return nil, nil
	}
	t.Cleanup(func() { prepareAuthoritativeWakeChild = oldPrepare })

	err := runCoopExec([]string{"--root", root, "--me", "codex", "sh"})
	if err == nil || !strings.Contains(err.Error(), "returned nil capability") {
		t.Fatalf("coop exec error = %v, want nil-capability refusal", err)
	}
}

func TestCoopWakeReadinessTempFailureDegradesOnlyWithoutRequiredOrLiveWake(t *testing.T) {
	cause := errors.New("TMPDIR unavailable")
	confirmedLive := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		IdentityConfirmed: true,
		Process:           wakeProcessInfo{Running: true},
	}
	tests := []struct {
		name       string
		require    bool
		inspection wakeLockInspection
		wantErr    bool
	}{
		{name: "optional without live wake degrades"},
		{name: "required wake fails closed", require: true, wantErr: true},
		{name: "confirmed live wake fails closed", inspection: confirmedLive, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			stderr := captureWakeStderr(t, func() {
				err = handleCoopWakeSetupFailure(tc.require, tc.inspection, "create wake readiness file", cause)
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && !strings.Contains(stderr, "TMPDIR unavailable") {
				t.Fatalf("degraded failure warning missing: %q", stderr)
			}
		})
	}
}

func TestCoopExecWakeInjectViaValidation(t *testing.T) {
	nonExecutable := filepath.Join(secureTempDirForTest(t), "injector")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write injector: %v", err)
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "blank inject via",
			args:    []string{"--wake-inject-via", "   ", "claude"},
			wantErr: "--wake-inject-via must not be blank",
		},
		{
			name:    "inject arg without inject via",
			args:    []string{"--wake-inject-arg", "exec", "claude"},
			wantErr: "--wake-inject-arg requires --wake-inject-via",
		},
		{
			name:    "non executable injector",
			args:    []string{"--wake-inject-via", nonExecutable, "claude"},
			wantErr: "not executable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runCoopExec(tt.args)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !containsStr(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestCoopExecWakeInjectModeValidation(t *testing.T) {
	injector := filepath.Join(secureTempDirForTest(t), "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "unknown mode",
			args:    []string{"--wake-inject-mode", "silent", "claude"},
			wantErr: "supported: auto, raw, paste, none",
		},
		{
			name:    "none with inject via",
			args:    []string{"--wake-inject-mode", "none", "--wake-inject-via", injector, "claude"},
			wantErr: "--wake-inject-via cannot be used with --wake-inject-mode none",
		},
		{
			name:    "none with inject arg",
			args:    []string{"--wake-inject-mode", "none", "--wake-inject-arg", "exec", "claude"},
			wantErr: "--wake-inject-arg cannot be used with --wake-inject-mode none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runCoopExec(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuildCoopWakeArgsDisablesInterruptAndGenericReuse(t *testing.T) {
	got := buildCoopWakeArgs("codex", "/tmp/root", "none", "", nil, "/tmp/ready")
	want := []string{
		"--no-update-check",
		"wake",
		"--me", "codex",
		"--root", "/tmp/root",
		"--baseline-existing",
		"--interrupt-cmd", "none",
		"--refuse-unverified-wake",
		"--inject-mode", "none",
		"--ready-file", "/tmp/ready",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCoopWakeArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildCoopWakeArgsNeverAcceptsExistingGenericWake(t *testing.T) {
	for _, mode := range []string{
		wakeInjectModeAuto,
		wakeInjectModeRaw,
		wakeInjectModePaste,
		wakeInjectModeNone,
	} {
		t.Run(mode, func(t *testing.T) {
			args := buildCoopWakeArgs("codex", "/tmp/root", mode, "", nil, "/tmp/ready")
			for _, arg := range args {
				if arg == "--accept-existing-wake" {
					t.Fatalf("generic mode %q permits ownerless wake reuse: %#v", mode, args)
				}
			}
		})
	}
}

func TestCleanupCoopWakeStartupHelperPreservesReusedClaim(t *testing.T) {
	root := t.TempDir()
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        5151,
		Generation: "reused-generation",
	})
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	stopped := false
	closed := false
	capability := &authoritativeWakeChildCapability{
		stop: func() error {
			stopped = true
			return nil
		},
		close: func() error {
			closed = true
			return nil
		},
	}
	waiter := &wakeProcessWaiter{done: make(chan struct{})}
	close(waiter.done)

	if err := cleanupCoopWakeStartupHelper(
		&os.Process{Pid: 5252},
		waiter,
		capability,
		nil,
		root,
		"codex",
		true,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if !stopped || !closed {
		t.Fatalf("startup helper cleanup stopped=%v closed=%v", stopped, closed)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("reused claim changed during startup-helper cleanup:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestCleanupCoopWakeStartupHelperSurfacesCapabilityFailures(t *testing.T) {
	stopErr := errors.New("stop failed")
	closeErr := errors.New("close failed")
	capability := &authoritativeWakeChildCapability{
		stop: func() error { return stopErr },
		close: func() error {
			return closeErr
		},
	}
	waiter := &wakeProcessWaiter{done: make(chan struct{})}
	close(waiter.done)

	err := cleanupCoopWakeStartupHelper(
		&os.Process{Pid: 5252},
		waiter,
		capability,
		nil,
		t.TempDir(),
		"codex",
		true,
		nil,
	)
	if !errors.Is(err, stopErr) || !errors.Is(err, closeErr) {
		t.Fatalf("cleanup error = %v, want joined stop and close failures", err)
	}
}

func TestFreshWakeHelperCleanupAttemptsStopWaitAndClose(t *testing.T) {
	stopErr := errors.New("stop failed")
	closeErr := errors.New("close failed")
	stopped := false
	closed := false
	capability := &authoritativeWakeChildCapability{
		stop: func() error {
			stopped = true
			return stopErr
		},
		close: func() error {
			closed = true
			return closeErr
		},
	}

	err := terminateAuthoritativeWakeHelperProcessForClaim(
		&os.Process{Pid: 5252},
		nil,
		capability,
		t.TempDir(),
		"codex",
		wakeOwner{},
		nil,
	)
	if !stopped || !closed {
		t.Fatalf("fresh cleanup stopped=%v closed=%v", stopped, closed)
	}
	if !errors.Is(err, stopErr) || !errors.Is(err, closeErr) ||
		!strings.Contains(err.Error(), "waiter is missing") {
		t.Fatalf("cleanup error = %v, want joined stop, wait, and close failures", err)
	}
}

func TestFreshWakeHelperCleanupPreservesChangedGeneration(t *testing.T) {
	root := t.TempDir()
	const wakePID = 5252
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        wakePID,
		Generation: "helper-generation",
	})
	expected := inspectWakeLock(root, "codex")
	capability := &authoritativeWakeChildCapability{
		stop: func() error {
			writeWakeLockForTest(t, root, "codex", wakeLock{
				PID:        wakePID,
				Generation: "replacement-generation",
			})
			return nil
		},
	}
	waiter := &wakeProcessWaiter{done: make(chan struct{})}
	close(waiter.done)

	if err := terminateAuthoritativeWakeHelperProcessForClaim(
		&os.Process{Pid: wakePID},
		waiter,
		capability,
		root,
		"codex",
		wakeOwner{},
		&expected,
	); err != nil {
		t.Fatal(err)
	}
	current := inspectWakeLock(root, "codex")
	if current.Lock.Generation != "replacement-generation" {
		t.Fatalf("changed generation was mutated during cleanup: %#v", current)
	}
}

func TestFreshWakeHelperCleanupRetainsStalePublishedGeneration(t *testing.T) {
	root := t.TempDir()
	const wakePID = 5252
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        wakePID,
		Generation: "published-before-exit",
	})
	current := inspectWakeLock(root, "codex")
	if current.Status != wakeLockStale {
		t.Fatalf("published helper fixture is %s, want stale: %#v", current.Status, current)
	}
	claim := exactCoopWakeHelperClaim(&os.Process{Pid: wakePID}, current)
	if claim == nil {
		t.Fatal("same-helper stale generation was not retained for exact cleanup")
	}
	capability := &authoritativeWakeChildCapability{
		stop:  func() error { return nil },
		close: func() error { return nil },
	}
	waiter := &wakeProcessWaiter{done: make(chan struct{})}
	close(waiter.done)
	owner := wakeOwner{}
	if err := cleanupCoopWakeStartupHelper(
		&os.Process{Pid: wakePID},
		waiter,
		capability,
		&owner,
		root,
		"codex",
		false,
		claim,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale same-helper generation survived exact cleanup: %v", err)
	}
}

func TestConfirmedLiveWakeRejectsStaleLockWithReusedPID(t *testing.T) {
	inspection := wakeLockInspection{
		Exists: true,
		Status: wakeLockStale,
		PID:    4242,
		Process: wakeProcessInfo{
			PID:     4242,
			Running: true,
		},
	}
	if confirmedLiveWake(inspection) {
		t.Fatal("stale wake lock with a reused live PID must not block coop degradation")
	}

	inspection.Status = wakeLockValid
	inspection.IdentityConfirmed = true
	if !confirmedLiveWake(inspection) {
		t.Fatal("confirmed valid live wake should block coop degradation")
	}
}

func TestCoopExecSessionInvalidName(t *testing.T) {
	err := runCoopExec([]string{"--session", "Bad/Name", "claude"})
	if err == nil {
		t.Fatal("expected error for invalid session name")
	}
	if !containsStr(err.Error(), "invalid session name") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCoopInitProvisionsDefaultExecSession(t *testing.T) {
	baseRoot := initCoopProjectForTest(t, "alice,bob")
	sessionRoot := filepath.Join(baseRoot, defaultSessionName)

	for _, agent := range []string{"alice", "bob"} {
		for _, leaf := range fsq.RequiredMailboxLeaves() {
			path := fsq.AgentMailboxPath(sessionRoot, agent, leaf)
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				t.Fatalf("coop init did not provision %s where default coop exec reads: info=%v err=%v", path, info, err)
			}
		}
		if info, err := os.Stat(fsq.AgentInboxNew(baseRoot, agent)); err != nil || !info.IsDir() {
			t.Fatalf("coop init did not preserve compatibility base mailbox for %q: info=%v err=%v", agent, info, err)
		}
	}

	sentinel := errors.New("exec sentinel")
	var execEnv []string
	oldExec := coopExecProcess
	coopExecProcess = func(_ string, _ []string, env []string) error {
		execEnv = append([]string(nil), env...)
		return sentinel
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	err := runCoopExec([]string{"--no-wake", "--me", "alice", "sh"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want sentinel", err)
	}
	if got := envValue(execEnv, envRoot); !sameTreeIdentity(got, sessionRoot) {
		t.Fatalf("coop exec AM_ROOT = %q, want provisioned session root %q", got, sessionRoot)
	}
}

func TestCoopInitBareSendListAndDrainAgree(t *testing.T) {
	initCoopProjectForTest(t, "alice,bob")

	sendOut, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--me", "bob",
			"--to", "alice",
			"--subject", "first-run agreement",
			"--body", "must remain readable",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("bare send after coop init: %v", err)
	}
	if !strings.Contains(sendOut, `"id"`) {
		t.Fatalf("bare send did not report success JSON: %q", sendOut)
	}

	listOut, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("bare list after successful send: %v", err)
	}
	if !strings.Contains(listOut, `"subject": "first-run agreement"`) {
		t.Fatalf("bare list did not return sent message: %q", listOut)
	}

	drainOut, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("bare drain after successful send: %v", err)
	}
	if !strings.Contains(drainOut, `"count": 1`) {
		t.Fatalf("bare drain did not consume sent message: %q", drainOut)
	}
}

func TestCoopInitRejectsPreexistingDefaultSessionSymlink(t *testing.T) {
	projectDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside target")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("create outside target: %v", err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	baseRoot := filepath.Join(projectDir, defaultCoopRoot)
	if err := os.Mkdir(baseRoot, 0o700); err != nil {
		t.Fatalf("create base root: %v", err)
	}
	sessionRoot := filepath.Join(baseRoot, defaultSessionName)
	if err := os.Symlink(outside, sessionRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err = captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--agents", "alice,bob", "--json", "--no-gitignore"}, false)
	})
	if err == nil ||
		!strings.Contains(err.Error(), "is a symlink") ||
		!strings.Contains(err.Error(), sessionRoot) ||
		!strings.Contains(err.Error(), "remove it") {
		t.Fatalf("coop init result = %v, want actionable session symlink refusal", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("read outside target: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("coop init followed collab symlink and mutated outside target: %v", entries)
	}
	info, lstatErr := os.Lstat(sessionRoot)
	if lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("collab symlink was changed: info=%v err=%v", info, lstatErr)
	}
}

func TestCoopExecRejectsPreexistingDefaultSessionSymlink(t *testing.T) {
	projectDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside target")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("create outside target: %v", err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	baseRoot := filepath.Join(projectDir, defaultCoopRoot)
	if err := fsq.EnsureRootDirs(baseRoot); err != nil {
		t.Fatalf("create base root: %v", err)
	}
	cfg := config.Config{Version: 1, Agents: []string{"alice", "bob"}}
	if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), cfg, false); err != nil {
		t.Fatalf("write base config: %v", err)
	}
	amqrcData, err := json.Marshal(amqrc{Root: defaultCoopRoot})
	if err != nil {
		t.Fatalf("marshal .amqrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), amqrcData, 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}
	sessionRoot := filepath.Join(baseRoot, defaultSessionName)
	if err := os.Symlink(outside, sessionRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	resolvedOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("resolve outside target: %v", err)
	}

	execCalled := false
	oldExec := coopExecProcess
	coopExecProcess = func(string, []string, []string) error {
		execCalled = true
		return errors.New("unexpected exec")
	}
	t.Cleanup(func() { coopExecProcess = oldExec })

	err = runCoopExec([]string{"--no-wake", "--me", "alice", "sh"})
	if err == nil ||
		!strings.Contains(err.Error(), "is a symlink") ||
		!strings.Contains(err.Error(), sessionRoot) ||
		!strings.Contains(err.Error(), "use: amq coop exec --root "+shellQuoteArg(resolvedOutside)+" --me alice sh") {
		t.Fatalf("coop exec result = %v, want actionable session symlink refusal", err)
	}
	if execCalled {
		t.Fatal("coop exec reached process replacement after session symlink refusal")
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("read outside target: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("coop exec followed collab symlink and mutated outside target: %v", entries)
	}
}

func TestCoopInitDefaultIncludesUser(t *testing.T) {
	projectDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	output, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--json"}, false)
	})
	if err != nil {
		t.Fatalf("runCoopInitInternal: %v", err)
	}
	var result struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal output: %v (output: %s)", err, output)
	}
	want := []string{"claude", "codex", "user"}
	if !reflect.DeepEqual(result.Agents, want) {
		t.Fatalf("agents = %#v, want %#v", result.Agents, want)
	}

	cfg, err := config.LoadConfig(filepath.Join(projectDir, defaultCoopRoot, "meta", "config.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg.Agents, want) {
		t.Fatalf("config agents = %#v, want %#v", cfg.Agents, want)
	}
	if _, err := os.Stat(filepath.Join(projectDir, defaultCoopRoot, defaultSessionName, "agents", "user", "inbox", "new")); err != nil {
		t.Fatalf("user inbox should be created: %v", err)
	}
}

func TestCoopInitSameDirectoryDifferentRootRequiresForce(t *testing.T) {
	_ = initCoopProjectForTest(t, "alice,bob")
	resetAmqrcCache()
	err := runCoopInitInternal([]string{"--root", "other-mail", "--json", "--no-gitignore"}, false)
	if err == nil || !strings.Contains(err.Error(), `already exists with root=".agent-mail"`) {
		t.Fatalf("different root without force = %v, want existing-root refusal", err)
	}
	resetAmqrcCache()
	if _, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--root", "other-mail", "--force", "--json", "--no-gitignore"}, false)
	}); err != nil {
		t.Fatalf("different root with force: %v", err)
	}
}

func TestCoopInitParentDirectoryRequiresForce(t *testing.T) {
	_ = initCoopProjectForTest(t, "alice,bob")
	parent, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(parent, "nested")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(child); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()
	err = runCoopInitInternal([]string{"--json", "--no-gitignore"}, false)
	if err == nil || !strings.Contains(err.Error(), "already initialized in parent directory") {
		t.Fatalf("nested init without force = %v, want parent refusal", err)
	}
}

func TestProvisionCoopSessionSymlinkHintRequiresAgentAndCommand(t *testing.T) {
	base := t.TempDir()
	if err := fsq.EnsureRootDirs(base); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sessionPath := filepath.Join(base, defaultSessionName)
	if err := os.Symlink(outside, sessionPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := provisionCoopSession(base, defaultSessionName, []string{"alice"}, "alice", "")
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("agent-only provision = %v, want symlink refusal", err)
	}
	if strings.Contains(err.Error(), "intentional relocation") {
		t.Fatal("empty command advertised relocation")
	}
	_, err = provisionCoopSession(base, defaultSessionName, []string{"alice"}, "", "sh")
	if err == nil || strings.Contains(err.Error(), "intentional relocation") {
		t.Fatalf("command-only provision = %v, want symlink refusal without relocation", err)
	}
	_, err = provisionCoopSession(base, defaultSessionName, []string{"alice"}, "alice", "sh")
	if err == nil || !strings.Contains(err.Error(), "intentional relocation") {
		t.Fatalf("agent+command provision = %v, want relocation hint", err)
	}
}

func TestCoopInitRerunUsesConfiguredAgents(t *testing.T) {
	root := initCoopProjectForTest(t, "alice,bob")
	cfgPath := filepath.Join(root, "meta", "config.json")
	configBefore, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read initial config: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, defaultSessionName, "agents")); err != nil {
		t.Fatalf("remove initial mailboxes: %v", err)
	}
	resetAmqrcCache()

	output, stderr, err := captureEnvOutput(t, func() error {
		return runCoopInitInternal([]string{"--json"}, false)
	})
	if err != nil {
		t.Fatalf("rerun coop init: %v", err)
	}
	if stderr != "" {
		t.Fatalf("rerun without explicit --agents wrote stderr: %q", stderr)
	}
	var result struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal rerun output: %v (output: %s)", err, output)
	}
	want := []string{"alice", "bob"}
	if !reflect.DeepEqual(result.Agents, want) {
		t.Fatalf("rerun agents = %#v, want configured %#v", result.Agents, want)
	}
	for _, agent := range want {
		if _, err := os.Stat(filepath.Join(root, defaultSessionName, "agents", agent, "inbox", "new")); err != nil {
			t.Fatalf("%s inbox was not restored: %v", agent, err)
		}
	}
	for _, agent := range []string{"claude", "codex"} {
		if _, err := os.Stat(filepath.Join(root, defaultSessionName, "agents", agent)); !os.IsNotExist(err) {
			t.Fatalf("rerun created default agent %q, stat err=%v", agent, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, defaultSessionName, "agents", reservedHumanHandle, "inbox", "new")); err != nil {
		t.Fatalf("rerun did not provision the reserved human inbox: %v", err)
	}
	configAfter, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config after rerun: %v", err)
	}
	if !bytes.Equal(configAfter, configBefore) {
		t.Fatalf("non-force rerun rewrote config:\nbefore=%s\nafter=%s", configBefore, configAfter)
	}
}

func TestCoopInitRerunWarnsWhenRequestedAgentsDifferWithoutForce(t *testing.T) {
	root := initCoopProjectForTest(t, "alice,bob")
	resetAmqrcCache()

	output, stderr, err := captureEnvOutput(t, func() error {
		return runCoopInitInternal([]string{"--agents", "carol,dave", "--json"}, false)
	})
	if err != nil {
		t.Fatalf("non-force coop init: %v", err)
	}
	const wantWarning = "warning: using existing config agents alice,bob; use --force to overwrite\n"
	if stderr != wantWarning {
		t.Fatalf("non-force warning = %q, want %q", stderr, wantWarning)
	}
	var result struct {
		Agents        []string `json:"agents"`
		ConfigWritten bool     `json:"config_written"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal non-force output: %v (output: %s)", err, output)
	}
	want := []string{"alice", "bob"}
	if !reflect.DeepEqual(result.Agents, want) || result.ConfigWritten {
		t.Fatalf("non-force result = %#v, want configured agents %#v without config rewrite", result, want)
	}
	if _, err := os.Stat(filepath.Join(root, defaultSessionName, "agents", "alice", "inbox", "new")); err != nil {
		t.Fatalf("non-force rerun did not retain configured mailbox: %v", err)
	}
	for _, agent := range []string{"carol", "dave"} {
		if _, err := os.Stat(filepath.Join(root, defaultSessionName, "agents", agent)); !os.IsNotExist(err) {
			t.Fatalf("non-force rerun created requested agent %q, stat err=%v", agent, err)
		}
	}
}

func TestCoopInitRerunDoesNotWarnWhenRequestedAgentsMatch(t *testing.T) {
	_ = initCoopProjectForTest(t, "alice,bob")
	resetAmqrcCache()

	output, stderr, err := captureEnvOutput(t, func() error {
		return runCoopInitInternal([]string{"--agents", "bob,alice,alice", "--json"}, false)
	})
	if err != nil {
		t.Fatalf("non-force coop init: %v", err)
	}
	if stderr != "" {
		t.Fatalf("matching explicit --agents wrote stderr: %q", stderr)
	}
	var result struct {
		Agents        []string `json:"agents"`
		ConfigWritten bool     `json:"config_written"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal non-force output: %v (output: %s)", err, output)
	}
	want := []string{"alice", "bob"}
	if !reflect.DeepEqual(result.Agents, want) || result.ConfigWritten {
		t.Fatalf("non-force result = %#v, want configured agents %#v without config rewrite", result, want)
	}
}

func TestCoopInitRerunRejectsInvalidExplicitAgentsBeforeMailboxMutation(t *testing.T) {
	root := initCoopProjectForTest(t, "alice,bob")
	if err := os.RemoveAll(filepath.Join(root, defaultSessionName, "agents")); err != nil {
		t.Fatalf("remove initial mailboxes: %v", err)
	}
	resetAmqrcCache()

	_, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--agents", "../invalid-handle", "--json"}, false)
	})
	if err == nil || !strings.Contains(err.Error(), "slashes not allowed") {
		t.Fatalf("invalid explicit agents result = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, defaultSessionName, "agents")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid explicit agents mutated mailboxes, stat err=%v", statErr)
	}
}

func TestCoopInitRerunWithoutConfigUsesRequestedAgents(t *testing.T) {
	root := initCoopProjectForTest(t, "alice,bob")
	cfgPath := filepath.Join(root, "meta", "config.json")
	if err := os.Remove(cfgPath); err != nil {
		t.Fatalf("remove initial config: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, defaultSessionName, "agents")); err != nil {
		t.Fatalf("remove initial mailboxes: %v", err)
	}
	resetAmqrcCache()

	output, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--agents", "carol,dave", "--json"}, false)
	})
	if err != nil {
		t.Fatalf("missing-config coop init: %v", err)
	}
	var result struct {
		Agents        []string `json:"agents"`
		ConfigWritten bool     `json:"config_written"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal missing-config output: %v (output: %s)", err, output)
	}
	want := []string{"carol", "dave"}
	if !reflect.DeepEqual(result.Agents, want) || !result.ConfigWritten {
		t.Fatalf("missing-config result = %#v, want requested agents %#v with config rewrite", result, want)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load recreated config: %v", err)
	}
	if !reflect.DeepEqual(cfg.Agents, want) {
		t.Fatalf("recreated config agents = %#v, want %#v", cfg.Agents, want)
	}
}

func TestCoopInitForceUsesRequestedAgents(t *testing.T) {
	root := initCoopProjectForTest(t, "alice,bob")
	resetAmqrcCache()
	output, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--force", "--agents", "carol,dave", "--json"}, false)
	})
	if err != nil {
		t.Fatalf("forced coop init: %v", err)
	}
	var result struct {
		Agents        []string `json:"agents"`
		ConfigWritten bool     `json:"config_written"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal forced output: %v (output: %s)", err, output)
	}
	want := []string{"carol", "dave"}
	if !reflect.DeepEqual(result.Agents, want) || !result.ConfigWritten {
		t.Fatalf("forced result = %#v, want agents %#v with config_written", result, want)
	}
	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err != nil {
		t.Fatalf("load forced config: %v", err)
	}
	if !reflect.DeepEqual(cfg.Agents, want) {
		t.Fatalf("forced config agents = %#v, want %#v", cfg.Agents, want)
	}
	for _, agent := range want {
		if _, err := os.Stat(filepath.Join(root, defaultSessionName, "agents", agent, "inbox", "new")); err != nil {
			t.Fatalf("%s inbox was not created: %v", agent, err)
		}
	}
}

func TestCoopInitRerunRejectsMalformedConfigBeforeMailboxMutation(t *testing.T) {
	root := initCoopProjectForTest(t, "alice,bob")
	cfgPath := filepath.Join(root, "meta", "config.json")
	const malformed = "{not-json\n"
	if err := os.WriteFile(cfgPath, []byte(malformed), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, defaultSessionName, "agents")); err != nil {
		t.Fatalf("remove initial mailboxes: %v", err)
	}
	resetAmqrcCache()

	_, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--json"}, false)
	})
	if err == nil || !strings.Contains(err.Error(), "failed to load existing config") {
		t.Fatalf("malformed config result = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, defaultSessionName, "agents")); !os.IsNotExist(statErr) {
		t.Fatalf("malformed rerun mutated mailboxes, stat err=%v", statErr)
	}
	data, readErr := os.ReadFile(cfgPath)
	if readErr != nil {
		t.Fatalf("read malformed config: %v", readErr)
	}
	if string(data) != malformed {
		t.Fatalf("malformed config was rewritten: %q", data)
	}
}

func TestCoopInitRerunRejectsInvalidConfiguredAgentsBeforeMailboxMutation(t *testing.T) {
	tests := []struct {
		name    string
		agents  []string
		wantErr string
	}{
		{name: "empty", wantErr: "existing config has no agents"},
		{name: "invalid handle", agents: []string{"alice", "../escape"}, wantErr: "invalid agent in existing config"},
		{name: "whitespace padded", agents: []string{" alice "}, wantErr: "invalid agent in existing config"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := initCoopProjectForTest(t, "alice,bob")
			cfgPath := filepath.Join(root, "meta", "config.json")
			cfg, err := config.LoadConfig(cfgPath)
			if err != nil {
				t.Fatalf("load initial config: %v", err)
			}
			cfg.Agents = test.agents
			if err := config.WriteConfig(cfgPath, cfg, true); err != nil {
				t.Fatalf("write invalid agents config: %v", err)
			}
			if err := os.RemoveAll(filepath.Join(root, defaultSessionName, "agents")); err != nil {
				t.Fatalf("remove initial mailboxes: %v", err)
			}
			resetAmqrcCache()

			_, err = captureEnvStdout(t, func() error {
				return runCoopInitInternal([]string{"--json"}, false)
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("invalid agents config result = %v, want %q", err, test.wantErr)
			}
			if _, statErr := os.Stat(filepath.Join(root, defaultSessionName, "agents")); !os.IsNotExist(statErr) {
				t.Fatalf("invalid agents rerun mutated mailboxes, stat err=%v", statErr)
			}
		})
	}
}

func TestCoopInitRerunRejectsUnreadableConfigBeforeMailboxMutation(t *testing.T) {
	root := initCoopProjectForTest(t, "alice,bob")
	cfgPath := filepath.Join(root, "meta", "config.json")
	if err := os.Remove(cfgPath); err != nil {
		t.Fatalf("remove initial config: %v", err)
	}
	if err := os.Mkdir(cfgPath, 0o700); err != nil {
		t.Fatalf("replace config with unreadable shape: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, defaultSessionName, "agents")); err != nil {
		t.Fatalf("remove initial mailboxes: %v", err)
	}
	resetAmqrcCache()

	_, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--json"}, false)
	})
	if err == nil || !strings.Contains(err.Error(), "failed to load existing config") {
		t.Fatalf("unreadable config result = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, defaultSessionName, "agents")); !os.IsNotExist(statErr) {
		t.Fatalf("unreadable rerun mutated mailboxes, stat err=%v", statErr)
	}
}

func TestCoopInitRerunRejectsDanglingConfigSymlinkBeforeMailboxMutation(t *testing.T) {
	projectDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	root := filepath.Join(projectDir, defaultCoopRoot)
	cfgPath := filepath.Join(root, "meta", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	const missingTarget = "missing-config-target.json"
	if err := os.Symlink(missingTarget, cfgPath); err != nil {
		t.Fatalf("create dangling config symlink: %v", err)
	}

	_, err = captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--agents", "carol", "--json"}, false)
	})
	if err == nil || !strings.Contains(err.Error(), "failed to load existing config") {
		t.Fatalf("dangling config result = %v", err)
	}
	for _, path := range []string{
		filepath.Join(root, defaultSessionName),
		filepath.Join(root, "threads"),
		filepath.Join(projectDir, ".amqrc"),
		filepath.Join(projectDir, ".gitignore"),
	} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("dangling-config init mutated %s, lstat err=%v", path, statErr)
		}
	}
	info, lstatErr := os.Lstat(cfgPath)
	if lstatErr != nil {
		t.Fatalf("lstat config symlink: %v", lstatErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config path mode = %v, want symlink", info.Mode())
	}
}

func initCoopProjectForTest(t *testing.T, agents string) string {
	t.Helper()
	projectDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if _, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--agents", agents, "--json"}, false)
	}); err != nil {
		t.Fatalf("first coop init: %v", err)
	}
	return filepath.Join(projectDir, defaultCoopRoot)
}

func TestCoopInitNextStepsDefaultAgentsSkipsUser(t *testing.T) {
	projectDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	output, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal(nil, true)
	})
	if err != nil {
		t.Fatalf("runCoopInitInternal: %v", err)
	}

	if !containsStr(output, "Terminal 1: amq coop exec claude") {
		t.Fatalf("missing Terminal 1 line for claude, output:\n%s", output)
	}
	if !containsStr(output, "Terminal 2: amq coop exec codex") {
		t.Fatalf("missing Terminal 2 line for codex, output:\n%s", output)
	}
	if !containsStr(output, "custom handle: amq coop exec --me <handle> <command>") {
		t.Fatalf("missing custom-handle hint line, output:\n%s", output)
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "Terminal") && strings.Contains(line, "user") {
			t.Fatalf("unexpected Terminal line mentioning reserved handle %q, output:\n%s", "user", output)
		}
	}
}

func TestCoopInitNextStepsThreeEngineAgentsSkipsUserKeepsContiguousNumbers(t *testing.T) {
	projectDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	output, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--agents", "claude,codex,grok,user"}, true)
	})
	if err != nil {
		t.Fatalf("runCoopInitInternal: %v", err)
	}

	if !containsStr(output, "Terminal 1: amq coop exec claude") {
		t.Fatalf("missing Terminal 1 line for claude, output:\n%s", output)
	}
	if !containsStr(output, "Terminal 2: amq coop exec codex") {
		t.Fatalf("missing Terminal 2 line for codex, output:\n%s", output)
	}
	if !containsStr(output, "Terminal 3: amq coop exec grok") {
		t.Fatalf("missing Terminal 3 line for grok, output:\n%s", output)
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "Terminal") && strings.Contains(line, "user") {
			t.Fatalf("unexpected Terminal line mentioning reserved handle %q, output:\n%s", "user", output)
		}
	}
}

func TestCoopInitNoGitignore(t *testing.T) {
	projectDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	output, err := captureEnvStdout(t, func() error {
		return runCoopInitInternal([]string{"--json", "--no-gitignore"}, false)
	})
	if err != nil {
		t.Fatalf("runCoopInitInternal: %v", err)
	}
	var result struct {
		GitignoreUpdated bool `json:"gitignore_updated"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal output: %v (output: %s)", err, output)
	}
	if result.GitignoreUpdated {
		t.Fatalf("gitignore_updated = true, want false with --no-gitignore")
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf(".gitignore should not be created with --no-gitignore (stat err: %v)", err)
	}
}

func TestCoopExecAutoInitNoGitignore(t *testing.T) {
	runCoopExecAutoInitNoGitignoreFixture(t)
}

func TestCoopExecAutoInitNoGitignoreLeavesInheritedCoopTreeUnchanged(t *testing.T) {
	ambientBase, ambientRoot := makeCoopAmbientSessionForTest(t, "current")
	rootID, baseRootID := treeIdentityTokens(ambientRoot, ambientBase)
	if rootID == "" || baseRootID == "" {
		t.Fatal("ambient fixture did not produce complete identity tokens")
	}
	t.Setenv(envRoot, ambientRoot)
	t.Setenv(envRootID, rootID)
	t.Setenv(envBaseRoot, ambientBase)
	t.Setenv(envBaseRootID, baseRootID)
	t.Setenv(envSession, "current")
	t.Setenv(envGlobalRoot, ambientBase)
	before := snapshotTreeDigest(t, ambientBase)

	projectDir := runCoopExecAutoInitNoGitignoreFixture(t)

	if after := snapshotTreeDigest(t, ambientBase); after != before {
		t.Fatalf("inherited AMQ tree mutated: before=%s after=%s", before, after)
	}
	fixtureAgent := fsq.AgentBase(
		filepath.Join(projectDir, defaultCoopRoot, defaultSessionName),
		"fixtureagent",
	)
	if info, err := os.Stat(fixtureAgent); err != nil || !info.IsDir() {
		t.Fatalf("isolated fixture mailbox missing: info=%v err=%v", info, err)
	}
}

func runCoopExecAutoInitNoGitignoreFixture(t *testing.T) string {
	t.Helper()
	clearCoopSessionPinForTest(t)
	setOptionalEnv(t, envRoot, "", false)
	setOptionalEnv(t, envGlobalRoot, "", false)
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	const gitignoreBefore = "# keep me\n"
	if err := os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte(gitignoreBefore), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}

	// Auto-init requires a resolvable binary: the provisioning guard resolves
	// the command before any filesystem effect, so the fixture runs a real
	// binary with the exec stubbed to a sentinel.
	sentinel := stubCoopExecSentinel(t)
	err = runCoopExec([]string{"--no-gitignore", "--no-wake", "-y", "--me", "fixtureagent", "sh"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want exec sentinel", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".amqrc")); err != nil {
		t.Fatalf(".amqrc should be created by coop exec auto-init: %v", err)
	}
	gitignoreAfter, err := os.ReadFile(filepath.Join(projectDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(gitignoreAfter) != gitignoreBefore {
		t.Fatalf(".gitignore changed with coop exec --no-gitignore:\n%s", gitignoreAfter)
	}
	return projectDir
}

func TestInitExplicitAgentsKeepsConfigLiteralAndProvisionsUser(t *testing.T) {
	root := t.TempDir()
	_, err := captureEnvStdout(t, func() error {
		return runInit([]string{"--root", root, "--agents", "claude,codex"})
	})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"claude", "codex"}
	if !reflect.DeepEqual(cfg.Agents, want) {
		t.Fatalf("config agents = %#v, want %#v", cfg.Agents, want)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "user", "inbox", "new")); err != nil {
		t.Fatalf("implicit user inbox should be created without changing config agents: %v", err)
	}
}

func TestCoopInitExplicitThreeEngineAgentsParses(t *testing.T) {
	root := t.TempDir()
	_, err := captureEnvStdout(t, func() error {
		return runInit([]string{"--root", root, "--agents", "claude,codex,grok,user"})
	})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"claude", "codex", "grok", "user"}
	if !reflect.DeepEqual(cfg.Agents, want) {
		t.Fatalf("config agents = %#v, want %#v", cfg.Agents, want)
	}
	for _, agent := range want {
		if _, err := os.Stat(filepath.Join(root, "agents", agent, "inbox", "new")); err != nil {
			t.Fatalf("%s inbox should be created: %v", agent, err)
		}
	}
}

func TestWakeReadyMessageReportsExistingWake(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "codex"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := t.TempDir()
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	got := wakeReadyMessage(root, "codex", 1000)
	if got != "Using existing amq wake (pid 4242)" {
		t.Fatalf("message = %q", got)
	}
}

func TestWakeReadyMessageReportsStartedWake(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "codex"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := t.TempDir()
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	got := wakeReadyMessage(root, "codex", wakePID)
	if got != "Started amq wake (pid 4242)" {
		t.Fatalf("message = %q", got)
	}
}

func sliceEq(a, b []string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && searchStr(s, sub)
}

func searchStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
