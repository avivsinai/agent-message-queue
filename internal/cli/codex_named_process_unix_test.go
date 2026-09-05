//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadCodexSessionMetaFiltersRootUserCLIThread(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		read bool
		root bool
	}{
		{name: "user cli", data: `{"type":"session_meta","payload":{"id":"thread-1","source":"cli","thread_source":"user"}}` + "\n", read: true, root: true},
		{name: "subagent object", data: `{"type":"session_meta","payload":{"id":"thread-2","source":{"subagent":{}},"thread_source":"subagent"}}` + "\n", read: true},
		{name: "wrong event", data: `{"type":"response_item","payload":{"id":"thread-3"}}` + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			identity, err := fileIdentity(path)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := readCodexSessionMeta(path, identity)
			if (err == nil) != test.read {
				t.Fatalf("readCodexSessionMeta error = %v, want success=%v", err, test.read)
			}
			if err != nil {
				return
			}
			if got := isCodexRootUserThread(meta); got != test.root {
				t.Fatalf("isCodexRootUserThread = %v, want %v", got, test.root)
			}
		})
	}
}

func TestReadCodexSessionMetaRejectsChangedFileIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	data := []byte(`{"type":"session_meta","payload":{"id":"thread-1","source":"cli","thread_source":"user"}}` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCodexSessionMeta(path, identity); err == nil || !strings.Contains(err.Error(), "no longer names") {
		t.Fatalf("readCodexSessionMeta error = %v, want identity refusal", err)
	}
}

func TestReadCodexSessionMetaIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", codexNamedMaxMetadata+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readCodexSessionMeta(path, identity); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("readCodexSessionMeta error = %v, want bounded refusal", err)
	}
}

func TestReadCodexSessionMetaRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := readCodexSessionMeta(path, identity); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("readCodexSessionMeta error = %v, want non-regular refusal", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO refusal took %s", elapsed)
	}
}

func TestWaitForCodexProcessThreadRetriesDelayedPersistence(t *testing.T) {
	oldLocate := locateCodexProcessThreadForWait
	oldPoll := codexNamedDiscoveryPoll
	t.Cleanup(func() {
		locateCodexProcessThreadForWait = oldLocate
		codexNamedDiscoveryPoll = oldPoll
	})
	codexNamedDiscoveryPoll = time.Millisecond
	calls := 0
	want := codexProcessThread{ThreadID: "thread-1", RolloutPath: "/synthetic/rollout.jsonl"}
	locateCodexProcessThreadForWait = func(context.Context, codexNamingTarget) (codexProcessThread, error) {
		calls++
		if calls < 3 {
			return codexProcessThread{}, errCodexThreadNotReady
		}
		return want, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := waitForCodexProcessThread(ctx, codexNamingTarget{})
	if err != nil {
		t.Fatalf("waitForCodexProcessThread: %v", err)
	}
	if got != want || calls != 3 {
		t.Fatalf("result = %#v after %d calls, want %#v after 3", got, calls, want)
	}
}

// An untouched composer is a valid session state, not a naming failure.
// Exercise the real wait loop through idle probes and a verified process exit.
func TestCodexNamingWaitsForIdleComposerAndStopsWhenProcessEnds(t *testing.T) {
	oldLocate, oldPoll, oldStart := locateCodexProcessThreadForWait, codexNamedDiscoveryPoll, startCodexNamingSidecar
	t.Cleanup(func() {
		locateCodexProcessThreadForWait, codexNamedDiscoveryPoll, startCodexNamingSidecar = oldLocate, oldPoll, oldStart
	})
	codexNamedDiscoveryPoll = time.Millisecond
	calls := 0
	locateCodexProcessThreadForWait = func(ctx context.Context, _ codexNamingTarget) (codexProcessThread, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > codexNamedProbeTimeout {
			t.Fatal("discovery probe has no bounded deadline")
		}
		calls++
		if calls < 4 {
			return codexProcessThread{}, errCodexThreadNotReady
		}
		return codexProcessThread{}, errCodexNamingTargetEnded
	}
	startCodexNamingSidecar = func(string) (*codexSidecar, error) {
		t.Fatal("idle composer caused a naming mutation")
		return nil, errors.New("unexpected")
	}
	if err := runCodexNamedSidecar("session1/codex", codexNamingTarget{}); err != nil {
		t.Fatalf("normal idle-then-exit produced a warning: %v", err)
	}
	if calls != 4 {
		t.Fatalf("discovery calls = %d, want 4", calls)
	}
}

func TestCodexNamingStopsOnVerifiedExitButReportsInspectionFailure(t *testing.T) {
	oldInspect := inspectCodexNamingProcess
	t.Cleanup(func() { inspectCodexNamingProcess = oldInspect })
	for _, tc := range []struct {
		name  string
		info  wakeProcessInfo
		ended bool
	}{
		{name: "exited", info: wakeProcessInfo{}, ended: true},
		{name: "reused PID", info: wakeProcessInfo{Running: true, StartToken: "replacement", BootID: "boot"}, ended: true},
		{name: "inspection failed", info: wakeProcessInfo{InspectError: errors.New("permission denied")}},
		{name: "missing identity", info: wakeProcessInfo{Running: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inspectCodexNamingProcess = func(int) wakeProcessInfo { return tc.info }
			_, err := validateCodexNamingTarget(codexNamingTarget{PID: 42, ProcessStart: "original", BootID: "boot"})
			if err == nil || errors.Is(err, errCodexNamingTargetEnded) != tc.ended {
				t.Fatalf("validation = %v, want ended=%v", err, tc.ended)
			}
		})
	}
}

func TestRunCodexNamedSidecarDoesNotStartRPCOnAmbiguousRoots(t *testing.T) {
	oldLocate := locateCodexProcessThreadForWait
	oldStart := startCodexNamingSidecar
	t.Cleanup(func() {
		locateCodexProcessThreadForWait = oldLocate
		startCodexNamingSidecar = oldStart
	})
	locateCodexProcessThreadForWait = func(context.Context, codexNamingTarget) (codexProcessThread, error) {
		return codexProcessThread{}, errors.New("2 root user Codex rollouts are open")
	}
	started := false
	startCodexNamingSidecar = func(string) (*codexSidecar, error) {
		started = true
		return nil, errors.New("unexpected")
	}
	err := runCodexNamedSidecar("session1/codex", codexNamingTarget{})
	if err == nil || !strings.Contains(err.Error(), "2 root user") {
		t.Fatalf("runCodexNamedSidecar error = %v, want ambiguity", err)
	}
	if started {
		t.Fatal("naming RPC started after ambiguous root discovery")
	}
}

func TestValidateCodexNamingTargetRejectsReplacedProcess(t *testing.T) {
	oldInspect := inspectCodexNamingProcess
	t.Cleanup(func() { inspectCodexNamingProcess = oldInspect })
	inspectCodexNamingProcess = func(int) wakeProcessInfo {
		return wakeProcessInfo{Running: true, StartToken: "replacement", BootID: "boot"}
	}
	_, err := validateCodexNamingTarget(codexNamingTarget{PID: 42, ProcessStart: "original", BootID: "boot"})
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("validateCodexNamingTarget error = %v, want replacement refusal", err)
	}
}

func TestCodexSidecarCallDeadlineInterruptsBlockedRead(t *testing.T) {
	script := filepath.Join(t.TempDir(), "codex-hang")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sidecar, err := startCodexSidecar(script)
	if err != nil {
		t.Fatalf("startCodexSidecar: %v", err)
	}
	defer sidecar.close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = sidecar.call(ctx, codexSidecarMessage{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: json.RawMessage(`{}`)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sidecar.call error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("blocked sidecar read returned after %s", elapsed)
	}
}

func TestCodexNamedSidecarProtocolSmokeUsesSyntheticStore(t *testing.T) {
	for _, concurrentRename := range []bool{false, true} {
		name := "unnamed thread"
		if concurrentRename {
			name = "manual rename during ownership probe"
		}
		t.Run(name, func(t *testing.T) { testCodexNativeNaming(t, concurrentRename) })
	}
}

func testCodexNativeNaming(t *testing.T, concurrentRename bool) {
	t.Helper()
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex executable is unavailable")
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 executable is unavailable")
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	initializeCodexStoreForTest(t, codex)
	threadID := "019d0000-0000-7000-8000-000000000001"
	rolloutDir := filepath.Join(home, "sessions", "2026", "09", "05")
	if err := os.MkdirAll(rolloutDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(rolloutDir, "rollout-2026-09-05T12-00-00-"+threadID+".jsonl")
	meta := map[string]any{
		"timestamp": "2026-09-05T12:00:00Z",
		"ordinal":   0,
		"type":      "session_meta",
		"payload": map[string]any{
			"session_id":        threadID,
			"id":                threadID,
			"timestamp":         "2026-09-05T12:00:00Z",
			"cwd":               "/tmp/amq-synthetic-project",
			"originator":        "codex-tui",
			"cli_version":       "test",
			"source":            "cli",
			"thread_source":     "user",
			"model_provider":    "openai",
			"base_instructions": map[string]any{"text": "synthetic"},
			"history_mode":      "legacy",
			"context_window":    map[string]any{"window_id": "019d0000-0000-7000-8000-000000000002"},
			"git": map[string]any{
				"commit_hash":    "synthetic",
				"branch":         "synthetic",
				"repository_url": "synthetic",
			},
		},
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	userEvent := []byte(`{"timestamp":"2026-09-05T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic"}]}}` + "\n")
	data = append(append(data, '\n'), userEvent...)
	if err := os.WriteFile(rollout, data, 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return strings.ReplaceAll(value, "'", "''") }
	statement := "INSERT INTO threads(id,rollout_path,created_at,updated_at,source,model_provider,cwd,title,sandbox_policy,approval_mode,has_user_event,cli_version,thread_source,preview,recency_at,recency_at_ms,name) VALUES('" +
		quote(threadID) + "','" + quote(rollout) + "',1770000000,1770000000,'cli','openai','/tmp/amq-synthetic-project','','{}','never',1,'test','user','synthetic',1770000000,1770000000000,NULL);"
	if output, err := exec.Command(sqlite, filepath.Join(home, "state_5.sqlite"), statement).CombinedOutput(); err != nil {
		t.Fatalf("insert synthetic Codex thread: %v: %s", err, output)
	}

	sidecar, err := startCodexSidecar(codex)
	if err != nil {
		t.Fatalf("startCodexSidecar: %v", err)
	}
	defer sidecar.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	initializeCodexSidecarForTest(t, ctx, sidecar)
	rolloutIdentity, err := fileIdentity(rollout)
	if err != nil {
		t.Fatal(err)
	}
	thread := codexProcessThread{ThreadID: threadID, RolloutPath: rollout, Identity: rolloutIdentity}
	wantName, wantRevalidations := "session1/codex", 3
	if concurrentRename {
		wantName, wantRevalidations = "user-custom-name", 2
	}
	revalidations := 0
	if err := setCodexThreadNameIfEmpty(ctx, sidecar, thread, "session1/codex", func() error {
		revalidations++
		if concurrentRename && revalidations == 2 {
			params, _ := json.Marshal(map[string]string{"threadId": thread.ThreadID, "name": wantName})
			_, err := sidecar.call(ctx, codexSidecarMessage{JSONRPC: "2.0", ID: 99, Method: "thread/name/set", Params: params})
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("setCodexThreadNameIfEmpty: %v", err)
	}
	if revalidations != wantRevalidations {
		t.Fatalf("ownership revalidations = %d, want %d", revalidations, wantRevalidations)
	}
	if name, err := readCodexThreadName(ctx, sidecar, thread, 4); err != nil || name != wantName {
		t.Fatalf("updated thread name = %q, err=%v", name, err)
	}
	if err := setCodexThreadNameIfEmpty(ctx, sidecar, thread, "replacement", func() error { return nil }); err != nil {
		t.Fatalf("preserve existing name: %v", err)
	}
	if name, err := readCodexThreadName(ctx, sidecar, thread, 5); err != nil || name != wantName {
		t.Fatalf("preserved thread name = %q, err=%v", name, err)
	}
}

func initializeCodexStoreForTest(t *testing.T, codex string) {
	t.Helper()
	sidecar, err := startCodexSidecar(codex)
	if err != nil {
		t.Fatalf("start initial Codex sidecar: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	initializeCodexSidecarForTest(t, ctx, sidecar)
	cancel()
	sidecar.close()
}

func initializeCodexSidecarForTest(t *testing.T, ctx context.Context, sidecar *codexSidecar) {
	t.Helper()
	request := codexSidecarMessage{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: json.RawMessage(`{"clientInfo":{"name":"amq-test","title":"AMQ Test","version":"1"},"capabilities":{}}`)}
	if _, err := sidecar.call(ctx, request); err != nil {
		t.Fatalf("initialize Codex sidecar: %v", err)
	}
	if err := sidecar.send(ctx, codexSidecarMessage{JSONRPC: "2.0", Method: "initialized", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("send initialized: %v", err)
	}
}
