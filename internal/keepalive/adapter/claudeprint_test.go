package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testClaudeUUID = "a616af69-92db-495e-9691-c512c80c4bd6"

type fakeProcess struct {
	pid             int
	waitErr         error
	exitImmediately bool
	killed          atomic.Bool
	killOnce        sync.Once
	killCh          chan struct{}
}

func newFakeProcess(pid int) *fakeProcess {
	return &fakeProcess{pid: pid, killCh: make(chan struct{})}
}

func (p *fakeProcess) PID() int { return p.pid }

func (p *fakeProcess) Wait() error {
	if p.exitImmediately {
		return p.waitErr
	}
	<-p.killCh
	return errors.New("killed")
}

func (p *fakeProcess) KillGroup() error {
	p.killed.Store(true)
	p.killOnce.Do(func() { close(p.killCh) })
	return nil
}

type fakeSpawner struct {
	calls   []processSpec
	proc    startedProcess
	err     error
	onStart func(processSpec)
}

func (f *fakeSpawner) Start(_ context.Context, spec processSpec) (startedProcess, error) {
	f.calls = append(f.calls, spec)
	if f.onStart != nil {
		f.onStart(spec)
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.proc == nil {
		f.proc = newFakeProcess(4242)
	}
	return f.proc, nil
}

func writeClaudePrintFixture(t *testing.T, uuid, cwd string) (configDir, stateDir, bin string) {
	t.Helper()
	configDir = t.TempDir()
	stateDir = t.TempDir()
	proj := filepath.Join(configDir, "projects", "-tmp-scratch")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(proj, uuid+".jsonl")
	rec, err := json.Marshal(map[string]string{"type": "user", "cwd": cwd, "sessionId": uuid})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonl, append(rec, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configDir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return configDir, stateDir, bin
}

func testClaudePrint(configDir, stateDir, bin string, runner CommandRunner, spawner processSpawner) ClaudePrint {
	return ClaudePrint{
		Runner:    runner,
		LookPath:  func(string) (string, error) { return bin, nil },
		Spawner:   spawner,
		Lock:      newMemoryLock(),
		ConfigDir: configDir,
		StateDir:  stateDir,
		AckWait:   2 * time.Second,
	}
}

func newMemoryLock() func(string) (func(), error) {
	var mu sync.Mutex
	held := map[string]struct{}{}
	return func(path string) (func(), error) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
		mu.Lock()
		defer mu.Unlock()
		if _, ok := held[path]; ok {
			return nil, errInjectBusy
		}
		held[path] = struct{}{}
		return func() {
			mu.Lock()
			delete(held, path)
			mu.Unlock()
		}, nil
	}
}

func writeInitThenAck(t *testing.T, path, payload string, beforeInit ...string) {
	t.Helper()
	var b strings.Builder
	for _, line := range beforeInit {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString(`{"type":"system","subtype":"init"}` + "\n")
	b.WriteString(`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":` + mustJSONString(payload) + `}]}}` + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Errorf("write ack log: %v", err)
	}
}

func TestDefaultRegistryRegistersClaudePrint(t *testing.T) {
	if _, err := DefaultRegistry().Get("claude-print"); err != nil {
		t.Fatalf("DefaultRegistry().Get(%q) failed: %v", "claude-print", err)
	}
	if _, err := DefaultRegistryWithLogf(func(string, ...any) {}).Get("claude-print"); err != nil {
		t.Fatalf("DefaultRegistryWithLogf().Get(%q) failed: %v", "claude-print", err)
	}
	cap := (ClaudePrint{}).Capability()
	if cap.Activation != ActivationNone || cap.Delivery != DeliverySubmitted || cap.Session != SessionExistingExact || cap.RequiresHuman {
		t.Fatalf("claude-print capability = %+v, want none+submitted+existing-exact+unattended", cap)
	}
	if !cap.Satisfies(Capability{Delivery: DeliverySubmitted, Session: SessionExistingExact}) {
		t.Fatal("claude-print does not satisfy submitted+existing-exact unattended min")
	}
}

func TestClaudePrintNormalizeTargetAcceptsSessionUUID(t *testing.T) {
	got, err := (ClaudePrint{}).NormalizeTarget(" " + claudePrintTargetSessionPrefix + testClaudeUUID + " ")
	if err != nil || got != claudePrintTargetSessionPrefix+testClaudeUUID {
		t.Fatalf("NormalizeTarget() = %q, %v", got, err)
	}
}

func TestClaudePrintTargetRejectsUnsafeOrMalformedIdentity(t *testing.T) {
	for _, target := range []string{
		"",
		"claude-print:new",
		"claude-print:session:",
		"claude-print:session:A616AF69-92DB-495E-9691-C512C80C4BD6",
		"claude-print:session:a616af69-92db-495e-9691-c512c80c4bd",
		"claude-print:session:../../../etc/passwd",
		"claude-print:session:a616af69-92db-495e-9691-c512c80c4bd6/extra",
		"claude-desktop:new",
		"codex-queue:thread:" + testClaudeUUID,
	} {
		if _, err := (ClaudePrint{}).NormalizeTarget(target); err == nil {
			t.Fatalf("NormalizeTarget(%q) succeeded; want refusal", target)
		}
		if _, err := (ClaudePrint{}).CapabilityForTarget(target); err == nil {
			t.Fatalf("CapabilityForTarget(%q) succeeded; want refusal", target)
		}
	}
}

func TestClaudePrintInjectUsesArgvOnlyAndKeepsPayloadOffArgv(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	payload := "hello & ; `quotes`\nnewline"
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{onStart: func(spec processSpec) {
		writeInitThenAck(t, spec.LogPath, payload)
	}}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	if err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, payload); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %#v, want help only", runner.calls)
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls[0].name != resolved || strings.Join(runner.calls[0].args, " ") != "--help" {
		t.Fatalf("probe help call = %#v, want resolved --help", runner.calls[0])
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(spawner.calls))
	}
	spec := spawner.calls[0]
	if spec.Path != resolved {
		t.Fatalf("spawn path = %q, want %q", spec.Path, resolved)
	}
	if spec.Dir != cwd {
		t.Fatalf("spawn cwd = %q, want %q", spec.Dir, cwd)
	}
	wantArgs := claudePrintArgv(testClaudeUUID)
	if strings.Join(spec.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("spawn args = %#v, want %#v", spec.Args, wantArgs)
	}
	for _, arg := range spec.Args {
		if strings.Contains(arg, payload) || strings.Contains(arg, "hello") {
			t.Fatalf("payload leaked into argv: %#v", spec.Args)
		}
	}
	var decoded struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(spec.Stdin, &decoded); err != nil {
		t.Fatalf("stdin json: %v (%q)", err, spec.Stdin)
	}
	if len(decoded.Message.Content) != 1 || decoded.Message.Content[0].Text != payload {
		t.Fatalf("stdin user text = %#v, want payload", decoded.Message.Content)
	}
	fp, ok := spawner.proc.(*fakeProcess)
	if !ok {
		t.Fatalf("proc type %T, want *fakeProcess", spawner.proc)
	}
	if fp.killed.Load() {
		t.Fatal("successful ack killed the child; want it left running")
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestClaudePrintAckIgnoresInitHooksAndDifferentReplayText(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	payload := "AMQ doorbell"
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{onStart: func(spec processSpec) {
		log := strings.Join([]string{
			`{"type":"system","subtype":"hook_started"}`,
			`{"type":"system","subtype":"init","session_id":"` + testClaudeUUID + `"}`,
			`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":"other payload"}]}}`,
			`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":"AMQ doorbell"}]}}`,
		}, "\n") + "\n"
		if err := os.WriteFile(spec.LogPath, []byte(log), 0o600); err != nil {
			t.Errorf("write log: %v", err)
		}
	}}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	if err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, payload); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
}

func TestClaudePrintLiveOwnerRefusesWithZeroSpawns(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	owner := map[string]any{
		"pid": os.Getpid(), "sessionId": testClaudeUUID, "kind": "interactive", "entrypoint": "cli",
	}
	raw, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "sessions", strconv.Itoa(os.Getpid())+".json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	err = a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("Inject() error = %v, want ErrTargetDegraded", err)
	}
	if !strings.Contains(err.Error(), "live owner pid") || !strings.Contains(err.Error(), "TTY seat") {
		t.Fatalf("error = %v, want live-owner remedy", err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawns = %#v, want zero", spawner.calls)
	}
}

func TestClaudePrintStaleOwnerPidIsIgnored(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	stalePID := 2147483647
	if alive, err := pidAlive(stalePID); err != nil || alive {
		t.Skipf("pid %d is unexpectedly alive", stalePID)
	}
	raw, err := json.Marshal(map[string]any{"pid": stalePID, "sessionId": testClaudeUUID, "kind": "interactive", "entrypoint": "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "sessions", strconv.Itoa(stalePID)+".json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{onStart: func(spec processSpec) {
		writeInitThenAck(t, spec.LogPath, "payload")
	}}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	if err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload"); err != nil {
		t.Fatalf("Inject() error = %v, want stale pid ignored", err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "sessions", strconv.Itoa(stalePID)+".json")); err != nil {
		t.Fatalf("stale pid file was removed; must leave it: %v", err)
	}
}

func TestClaudePrintMissingJSONLIsTargetNotFound(t *testing.T) {
	configDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, "projects", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configDir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	a := testClaudePrint(configDir, t.TempDir(), bin, runner, &fakeSpawner{})
	err := a.Probe(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID)
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe() error = %v, want ErrTargetNotFound", err)
	}
}

func TestClaudePrintGoneCwdRefusesWithoutSpawn(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, missing)
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if err == nil {
		t.Fatal("Inject() succeeded with missing cwd")
	}
	if !strings.Contains(err.Error(), "recorded cwd") {
		t.Fatalf("error = %v, want cwd remedy", err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawns = %#v, want zero", spawner.calls)
	}
}

func TestClaudePrintAckTimeoutKillsProcessGroup(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	proc := newFakeProcess(99)
	spawner := &fakeSpawner{proc: proc}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	a.AckWait = 50 * time.Millisecond
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if err == nil {
		t.Fatal("Inject() succeeded on ack timeout")
	}
	if !strings.Contains(err.Error(), "ack timeout") {
		t.Fatalf("error = %v, want ack timeout", err)
	}
	if !proc.killed.Load() {
		t.Fatal("timeout did not KillGroup")
	}
}

func TestClaudePrintAckDuringKillIsUncertain(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	inner := newFakeProcess(99)
	proc := &ackOnKillProcess{fakeProcess: inner, payload: "payload"}
	spawner := &fakeSpawner{proc: proc, onStart: func(spec processSpec) {
		proc.logPath = spec.LogPath
		if err := os.WriteFile(spec.LogPath, []byte(`{"type":"system","subtype":"init"}`+"\n"), 0o600); err != nil {
			t.Errorf("write init: %v", err)
		}
	}}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	a.AckWait = 50 * time.Millisecond
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if !errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("Inject() error = %v, want ErrInjectUncertain", err)
	}
	if !inner.killed.Load() {
		t.Fatal("KillGroup was not called")
	}
}

type ackOnKillProcess struct {
	*fakeProcess
	logPath string
	payload string
}

func (p *ackOnKillProcess) KillGroup() error {
	line := `{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":` + mustJSONString(p.payload) + `}]}}` + "\n"
	if p.logPath != "" {
		f, err := os.OpenFile(p.logPath, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
	return p.fakeProcess.KillGroup()
}

func TestLastJSONLCwdReadsTailOfLargeSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"type":"pad"}` + "\n")
	n := claudePrintJSONLCwdTail/len(line) + 50
	for i := 0; i < n; i++ {
		if _, err := f.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	want := "/private/tmp/amq-w6-tail-cwd"
	rec, err := json.Marshal(map[string]string{"type": "user", "cwd": want})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(rec, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= claudePrintJSONLCwdTail {
		t.Fatalf("fixture size %d, want > %d", info.Size(), claudePrintJSONLCwdTail)
	}
	got, err := lastJSONLCwd(path)
	if err != nil || got != want {
		t.Fatalf("lastJSONLCwd() = %q, %v, want %q", got, err, want)
	}
}

func TestClaudePrintChildExitBeforeAckIsReplayable(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	proc := newFakeProcess(7)
	proc.exitImmediately = true
	proc.waitErr = errors.New("exit status 1")
	spawner := &fakeSpawner{proc: proc, onStart: func(spec processSpec) {
		if err := os.WriteFile(spec.LogPath, []byte(`{"type":"system","subtype":"init"}`+"\n"), 0o600); err != nil {
			t.Errorf("write log: %v", err)
		}
	}}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if err == nil {
		t.Fatal("Inject() succeeded when child exited before ack")
	}
	if errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("Inject() = %v, want replayable not uncertain", err)
	}
	if !strings.Contains(err.Error(), "exited before ack") {
		t.Fatalf("error = %v, want exited before ack", err)
	}
}

func TestClaudePrintLookPathMissMakesZeroCalls(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, _ := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{}
	a := ClaudePrint{
		Runner:    runner,
		LookPath:  func(string) (string, error) { return "", errors.New("not in PATH") },
		Spawner:   spawner,
		ConfigDir: configDir,
		StateDir:  stateDir,
	}
	if err := a.Probe(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID); err == nil {
		t.Fatal("Probe() succeeded with LookPath miss")
	} else if !strings.Contains(err.Error(), "put claude on PATH") {
		t.Fatalf("error = %v, want PATH remedy", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls = %#v, want zero", runner.calls)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawns = %#v, want zero", spawner.calls)
	}
}

func TestClaudePrintHelpWithoutResumeIsRefused(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	runner := &fakeCommandRunner{output: []byte("Usage: claude chat\n")}
	spawner := &fakeSpawner{}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	err := a.Probe(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID)
	if err == nil {
		t.Fatal("Probe() succeeded without --resume in help")
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawns = %#v, want zero", spawner.calls)
	}
}

func TestClaudePrintExecutesResolvedSymlinkTarget(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, _ := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	dir := t.TempDir()
	target := filepath.Join(dir, "claude-real")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "claude")
	if err := os.Symlink(target, shim); err != nil {
		t.Skipf("symlink: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(shim)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{onStart: func(spec processSpec) {
		writeInitThenAck(t, spec.LogPath, "x")
	}}
	a := testClaudePrint(configDir, stateDir, shim, runner, spawner)
	if err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "x"); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if runner.calls[0].name != resolved || spawner.calls[0].Path != resolved {
		t.Fatalf("help=%q spawn=%q, want resolved %q", runner.calls[0].name, spawner.calls[0].Path, resolved)
	}
}

func TestClaudePrintBusyInjectLockRefusesSecondSpawn(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	started := make(chan struct{})
	release := make(chan struct{})
	proc := newFakeProcess(1)
	spawner := &fakeSpawner{proc: proc, onStart: func(spec processSpec) {
		writeInitThenAck(t, spec.LogPath, "payload")
		close(started)
		<-release
	}}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first Inject() did not reach spawn")
	}
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "other")
	if !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("second Inject() error = %v, want ErrTargetDegraded", err)
	}
	if !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("second Inject() error = %v, want in progress", err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawns = %d, want 1 (busy inject must not spawn)", len(spawner.calls))
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("first Inject() error = %v", err)
	}
}

func TestClaudePrintReplayBeforeInitIsNotAck(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{onStart: func(spec processSpec) {
		if err := os.WriteFile(spec.LogPath, []byte(
			`{"type":"user","message":{"content":[{"type":"text","text":"payload"}]},"isReplay":true}`+"\n"+
				`{"type":"system","subtype":"init"}`+"\n",
		), 0o600); err != nil {
			t.Errorf("write log: %v", err)
		}
	}}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	a.AckWait = 80 * time.Millisecond
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if err == nil {
		t.Fatal("Inject() treated a pre-init isReplay as ack")
	}
	if errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("Inject() = %v, want timeout not uncertain", err)
	}
	if !strings.Contains(err.Error(), "ack timeout") {
		t.Fatalf("error = %v, want ack timeout", err)
	}
}

func TestClaudePrintMalformedOwnerFileRefusesZeroSpawns(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	if err := os.WriteFile(filepath.Join(configDir, "sessions", "424242.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("Inject() error = %v, want ErrTargetDegraded", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(configDir, "sessions", "424242.json")) {
		t.Fatalf("Inject() error = %v, want named owner file", err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawns = %d, want 0", len(spawner.calls))
	}
}

func TestClaudePrintPidMismatchOwnerRefusesZeroSpawns(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	if err := os.WriteFile(filepath.Join(configDir, "sessions", "9.json"), []byte(`{"pid":8,"sessionId":"`+testClaudeUUID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("Inject() error = %v, want ErrTargetDegraded", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(configDir, "sessions", "9.json")) {
		t.Fatalf("Inject() error = %v, want named owner file", err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawns = %d, want 0", len(spawner.calls))
	}
}

func TestClaudePrintNegativePidOwnerRefusesZeroSpawns(t *testing.T) {
	cwd := t.TempDir()
	configDir, stateDir, bin := writeClaudePrintFixture(t, testClaudeUUID, cwd)
	if err := os.WriteFile(filepath.Join(configDir, "sessions", "-1.json"), []byte(`{"pid":-1,"sessionId":"`+testClaudeUUID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{output: []byte("Usage: --resume --output-format --replay-user-messages\n")}
	spawner := &fakeSpawner{}
	a := testClaudePrint(configDir, stateDir, bin, runner, spawner)
	err := a.Inject(context.Background(), claudePrintTargetSessionPrefix+testClaudeUUID, "payload")
	if !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("Inject() error = %v, want ErrTargetDegraded", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(configDir, "sessions", "-1.json")) {
		t.Fatalf("Inject() error = %v, want named owner file", err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawns = %d, want 0", len(spawner.calls))
	}
}
