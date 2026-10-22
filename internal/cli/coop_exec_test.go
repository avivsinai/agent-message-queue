//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

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

func putBareCommandOnPATH(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCoopExecNamedDefaultsOn(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	putBareCommandOnPATH(t, "claude")

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
		"--me", "coder1",
		"--no-wake",
		"claude",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want sentinel", err)
	}
	wantArgv := []string{"claude", "--name", "coder1"}
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", gotArgv, wantArgv)
	}
}

func TestInjectCoopNamedSlashCommand(t *testing.T) {
	oldInject := tiocstiInject
	oldSleep := rawInjectSleep
	oldReady := coopNamedTTYReady
	t.Cleanup(func() {
		tiocstiInject = oldInject
		rawInjectSleep = oldSleep
		coopNamedTTYReady = oldReady
	})
	rawInjectSleep = func(time.Duration) {}
	coopNamedTTYReady = func() bool { return true }

	var injected []string
	tiocstiInject = func(text string) error {
		injected = append(injected, text)
		return nil
	}

	if err := injectCoopNamedSlashCommand("coder1", "codex"); err != nil {
		t.Fatalf("injectCoopNamedSlashCommand: %v", err)
	}
	want := []string{"/rename coder1", "\r"}
	if !reflect.DeepEqual(injected, want) {
		t.Fatalf("injected = %#v, want %#v", injected, want)
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
