//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

func TestCoopExecDeferWakeRejectsActiveWakeOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--defer-wake", "--no-wake", "claude"},
		{"--defer-wake", "--require-wake", "claude"},
		{"--defer-wake", "--wake-inject-mode", "none", "claude"},
		{"--defer-wake", "--wake-inject-via", "/tmp/injector", "claude"},
		{"--defer-wake", "--wake-inject-arg", "exec", "claude"},
	} {
		err := runCoopExec(args)
		if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("runCoopExec(%v) error = %v, want defer-wake conflict", args, err)
		}
	}
}

func TestCoopExecDeferWakeExportsManifestBeforeExec(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	writeWakeTestMessage(t, root, "codex", "stale.md", "stale-id", "private subject", "private body")
	self := os.Getpid()
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != self {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "exec-start",
			BootID:     "exec-boot",
		}
	})
	stubWakeProcessSID(t, func(pid int) (int, error) {
		if pid != self {
			return 0, fmt.Errorf("unexpected pid %d", pid)
		}
		return 77, nil
	})
	t.Setenv(envWakeOwner, `{"pid":999,"process_start":"inherited","boot_id":"old-boot"}`)

	oldExec := coopExecProcess
	defer func() { coopExecProcess = oldExec }()
	sentinel := errors.New("exec intercepted")
	coopExecProcess = func(_ string, _ []string, env []string) error {
		path := envValue(env, envWakeBaselineFile)
		if path == "" || envValue(env, envWakeBaselineError) != "" {
			t.Fatalf("deferred env missing manifest or contains error: %#v", env)
		}
		baseline, err := readWakeBaseline(path, root, "codex")
		if err != nil {
			t.Fatalf("readWakeBaseline before exec: %v", err)
		}
		if _, ok := baseline.IDs["stale-id"]; !ok {
			t.Fatalf("baseline ids = %#v", baseline.IDs)
		}
		var owner wakeOwner
		if err := json.Unmarshal([]byte(envValue(env, envWakeOwner)), &owner); err != nil {
			t.Fatalf("decode deferred owner: %v", err)
		}
		if owner.PID != self || owner.ProcessStart != "exec-start" || owner.BootID != "exec-boot" || owner.SessionID != 77 {
			t.Fatalf("deferred owner = %#v, want exact exec identity", owner)
		}
		return sentinel
	}
	err := runCoopExec([]string{"--defer-wake", "--no-init", "--root", root, "--me", "codex", "true"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("runCoopExec error = %v", err)
	}
}

func TestCoopExecDeferWakeFailsBeforeExecWithoutExactOwner(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: pid == self, StartToken: "exec-start"}
	})

	oldExec := coopExecProcess
	defer func() { coopExecProcess = oldExec }()
	called := false
	coopExecProcess = func(_ string, _ []string, _ []string) error {
		called = true
		return nil
	}

	err := runCoopExec([]string{"--defer-wake", "--no-init", "--root", root, "--me", "codex", "true"})
	if err == nil || !strings.Contains(err.Error(), "wake owner boot id is required") {
		t.Fatalf("runCoopExec error = %v, want missing exact boot identity", err)
	}
	if called {
		t.Fatal("exec ran without an exact deferred owner")
	}
}

func TestCoopExecDeferWakeCaptureFailureStillExecsWithSanitizedError(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxWakeBaselineMessages; i++ {
		name := fmt.Sprintf("stale-%04d.md", i)
		if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, "codex"), name), []byte("invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	oldExec := coopExecProcess
	defer func() { coopExecProcess = oldExec }()
	sentinel := errors.New("exec intercepted")
	coopExecProcess = func(_ string, _ []string, env []string) error {
		if got := envValue(env, envWakeBaselineFile); got != "" {
			t.Fatalf("baseline file = %q, want empty", got)
		}
		if got := envValue(env, envWakeBaselineError); got != wakeBaselineCaptureError {
			t.Fatalf("baseline error = %q, want sanitized code", got)
		}
		return sentinel
	}
	err := runCoopExec([]string{"--defer-wake", "--no-init", "--root", root, "--me", "codex", "true"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("runCoopExec error = %v", err)
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

func TestBuildCoopWakeArgsIncludesNoneMode(t *testing.T) {
	got := buildCoopWakeArgs("codex", "/tmp/root", "none", "", nil, "/tmp/ready")
	want := []string{"--no-update-check", "wake", "--me", "codex", "--root", "/tmp/root", "--baseline-existing", "--inject-mode", "none", "--ready-file", "/tmp/ready", "--accept-existing-wake"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCoopWakeArgs() = %#v, want %#v", got, want)
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
	if _, err := os.Stat(filepath.Join(projectDir, defaultCoopRoot, "agents", "user", "inbox", "new")); err != nil {
		t.Fatalf("user inbox should be created: %v", err)
	}
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

	err = runCoopExec([]string{"--no-gitignore", "--no-wake", "-y", "definitely-missing-amq-test-binary"})
	if err == nil {
		t.Fatal("expected command lookup error")
	}
	if !containsStr(err.Error(), "command not found") {
		t.Fatalf("unexpected error: %v", err)
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
}

func TestInitExplicitAgentsDoesNotInjectUser(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(root, "agents", "user")); !os.IsNotExist(err) {
		t.Fatalf("user mailbox should not be created by explicit init, stat err=%v", err)
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
