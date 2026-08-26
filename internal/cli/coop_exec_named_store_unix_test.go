//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoopNamedSessionLabel(t *testing.T) {
	for _, test := range []struct {
		session string
		handle  string
		want    string
	}{
		{session: "", handle: "codex", want: "codex"},
		{session: "feature-x", handle: "codex", want: "feature-x/codex"},
	} {
		if got := coopNamedSessionLabel(test.session, test.handle); got != test.want {
			t.Fatalf("coopNamedSessionLabel(%q, %q) = %q, want %q", test.session, test.handle, got, test.want)
		}
	}
}

func TestAgentArgsPreventAutoName(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "explicit name", args: []string{"--name", "custom"}, want: true},
		{name: "resume", args: []string{"--resume", "thread"}, want: true},
		{name: "continue", args: []string{"-c"}, want: true},
		{name: "model value looks like name flag", args: []string{"--model", "--name"}},
		{name: "end of options", args: []string{"--", "--name"}},
		{name: "name after model value", args: []string{"--model", "opus", "--name", "custom"}, want: true},
		{name: "name in model equals value", args: []string{"--model=--name"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := agentArgsPreventAutoName(test.args); got != test.want {
				t.Fatalf("agentArgsPreventAutoName(%#v) = %v, want %v", test.args, got, test.want)
			}
		})
	}
}

func TestResolveCoopNamedEnabledPrecedence(t *testing.T) {
	t.Setenv("AMQ_COOP_NAMED", "0")
	got, err := resolveCoopNamedEnabled(false, true)
	if err != nil || got {
		t.Fatalf("environment off switch = %v, %v", got, err)
	}
	got, err = resolveCoopNamedEnabled(true, true)
	if err != nil || !got {
		t.Fatalf("flag precedence = %v, %v", got, err)
	}

	t.Setenv("AMQ_COOP_NAMED", "bad")
	if _, err := resolveCoopNamedEnabled(false, true); err == nil || GetExitCode(err) != ExitUsage {
		t.Fatalf("invalid environment value error = %v", err)
	}
}

func TestResolveCoopNamedEnabledUsesLaunchConfig(t *testing.T) {
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"schema":1,"named":false,"agents":[{"handle":"claude","adapter":"claude","command":["claude"]}]}`)
	if err := os.WriteFile(filepath.Join(project, ".amq", "launch.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	previous, wasSet := os.LookupEnv("AMQ_COOP_NAMED")
	if err := os.Unsetenv("AMQ_COOP_NAMED"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv("AMQ_COOP_NAMED", previous)
		} else {
			_ = os.Unsetenv("AMQ_COOP_NAMED")
		}
	})
	got, err := resolveCoopNamedEnabled(false, true)
	if err != nil || got {
		t.Fatalf("launch config off switch = %v, %v", got, err)
	}
}

func TestCodexNamedStoreReaderUsesReadOnlyStoreAndFiltersWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	rollout := filepath.Join(home, "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	oldQuery := runCodexSQLiteQuery
	t.Cleanup(func() { runCodexSQLiteQuery = oldQuery })
	var gotDB string
	runCodexSQLiteQuery = func(dbPath string) ([]byte, error) {
		gotDB = dbPath
		return []byte(`[{
  "cwd": "` + cwd + `",
  "name": "",
  "rollout_path": "` + rollout + `"
}]`), nil
	}
	start := time.Now().Add(-time.Second)
	candidate, err := (codexNamedStoreReader{}).locate(cwd, start)
	if err != nil {
		t.Fatalf("locate Codex candidate: %v", err)
	}
	if candidate.storePath != filepath.Join(home, codexStateFilename) || candidate.key != rollout {
		t.Fatalf("candidate = %#v", candidate)
	}
	if gotDB != candidate.storePath {
		t.Fatalf("sqlite database = %q, want %q", gotDB, candidate.storePath)
	}
	runCodexSQLiteQuery = func(string) ([]byte, error) {
		return []byte(`[{
  "cwd": "` + cwd + `",
  "name": "",
  "title": "feature/codex",
  "rollout_path": "` + rollout + `"
}]`), nil
	}
	if got, err := (codexNamedStoreReader{}).readName(candidate); err != nil || got != "feature/codex" {
		t.Fatalf("legacy Codex title readback = %q, %v", got, err)
	}

	runCodexSQLiteQuery = func(string) ([]byte, error) {
		return []byte(`[{
  "cwd": "` + cwd + `",
  "name": "",
  "rollout_path": "` + rollout + `"
}, {
  "cwd": "` + cwd + `",
  "name": "other",
  "rollout_path": "` + rollout + ".second" + `"
}]`), nil
	}
	second := rollout + ".second"
	if err := os.WriteFile(second, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (codexNamedStoreReader{}).locate(cwd, start); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("multiple candidates error = %v", err)
	}
}

func TestCodexNamedStoreReaderSkipsCWDAndOldRollouts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	current := filepath.Join(home, "current.jsonl")
	other := filepath.Join(home, "other.jsonl")
	old := filepath.Join(home, "old.jsonl")
	for _, path := range []string{current, other, old} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := start.Add(-time.Second)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	rows, err := json.Marshal([]codexThreadRow{
		{CWD: cwd, RolloutPath: current},
		{CWD: filepath.Join(home, "other-cwd"), RolloutPath: other},
		{CWD: cwd, RolloutPath: old},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldQuery := runCodexSQLiteQuery
	t.Cleanup(func() { runCodexSQLiteQuery = oldQuery })
	runCodexSQLiteQuery = func(string) ([]byte, error) { return rows, nil }
	candidate, err := (codexNamedStoreReader{}).locate(cwd, start)
	if err != nil {
		t.Fatalf("locate filtered Codex candidate: %v", err)
	}
	if candidate.key != current {
		t.Fatalf("candidate = %#v, want current rollout", candidate)
	}
}

func TestCodexNamedStoreReaderMissingSQLiteIsUnknown(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	_, err = (codexNamedStoreReader{}).locate(cwd, time.Now())
	if err == nil || !strings.Contains(err.Error(), "sqlite3") {
		t.Fatalf("missing sqlite3 error = %v", err)
	}
}

func TestRunCodexSQLiteQueryProcessUsesFixedReadOnlyQuery(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	script := filepath.Join(dir, "sqlite3")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$AMQ_TEST_SQLITE_ARGS\"\nprintf '[]\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("AMQ_TEST_SQLITE_ARGS", argsPath)
	dbPath := filepath.Join(dir, "database with spaces")
	data, err := runCodexSQLiteQueryProcess(dbPath)
	if err != nil {
		t.Fatalf("run sqlite3 query: %v", err)
	}
	if string(data) != "[]\n" {
		t.Fatalf("sqlite3 output = %q", data)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "-readonly\n-json\n" + dbPath + "\n" + codexThreadsQuery + "\n"
	if string(args) != want {
		t.Fatalf("sqlite3 args = %q, want %q", args, want)
	}
}

func TestReadCodexThreadRowsRejectsSchemaMismatch(t *testing.T) {
	oldQuery := runCodexSQLiteQuery
	t.Cleanup(func() { runCodexSQLiteQuery = oldQuery })
	runCodexSQLiteQuery = func(string) ([]byte, error) {
		return []byte(`[{"cwd":"cwd","name":"name","rollout_path":"rollout","unexpected":true}]`), nil
	}
	if _, err := readCodexThreadRows("ignored"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("schema mismatch error = %v", err)
	}
}

func TestCursorNamedStoreReaderFiltersCWDAndWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(home, ".cursor", "chats", "workspace", "chat", "meta.json")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte(`{"title":"","cwd":"`+cwd+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := (cursorNamedStoreReader{}).locate(cwd, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("locate Cursor candidate: %v", err)
	}
	if candidate.storePath != metaPath || candidate.name != "" {
		t.Fatalf("candidate = %#v", candidate)
	}
	if got, err := (cursorNamedStoreReader{}).readName(candidate); err != nil || got != "" {
		t.Fatalf("Cursor readback = %q, %v", got, err)
	}

	other := filepath.Join(home, ".cursor", "chats", "workspace", "other", "meta.json")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{"title":"other","cwd":"`+cwd+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (cursorNamedStoreReader{}).locate(cwd, time.Now().Add(-time.Second)); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("multiple Cursor candidates error = %v", err)
	}
}

type fakeCoopNamedStoreReader struct {
	candidate coopNamedStoreCandidate
	locateErr error
	names     []string
	readCalls int
}

func (r *fakeCoopNamedStoreReader) locate(string, time.Time) (coopNamedStoreCandidate, error) {
	if r.locateErr != nil {
		return coopNamedStoreCandidate{}, r.locateErr
	}
	return r.candidate, nil
}

func (r *fakeCoopNamedStoreReader) readName(coopNamedStoreCandidate) (string, error) {
	r.readCalls++
	if len(r.names) == 0 {
		return "", nil
	}
	index := r.readCalls - 1
	if index >= len(r.names) {
		index = len(r.names) - 1
	}
	return r.names[index], nil
}

func TestRunCoopNamedTUISkipsNamedStoreAndConfirmsReadback(t *testing.T) {
	oldInject := coopNamedTTYInject
	oldTimeout := coopNamedTUIReadbackTimeout
	oldSleep := coopNamedReadbackSleep
	t.Cleanup(func() {
		coopNamedTTYInject = oldInject
		coopNamedTUIReadbackTimeout = oldTimeout
		coopNamedReadbackSleep = oldSleep
	})

	reader := &fakeCoopNamedStoreReader{candidate: coopNamedStoreCandidate{name: "existing"}}
	injections := 0
	coopNamedTTYInject = func(string, string) error {
		injections++
		return nil
	}
	_, stderr, err := captureEnvOutput(t, func() error {
		return runCoopNamedTUI(reader, "feature/codex", "codex", "/tmp/project", time.Now())
	})
	if err != nil || injections != 0 || stderr != "" {
		t.Fatalf("named store skip: err=%v injections=%d stderr=%q", err, injections, stderr)
	}

	reader = &fakeCoopNamedStoreReader{candidate: coopNamedStoreCandidate{}, names: []string{"", "feature/codex"}}
	coopNamedTUIReadbackTimeout = time.Second
	coopNamedReadbackSleep = func(time.Duration) {}
	_, stderr, err = captureEnvOutput(t, func() error {
		return runCoopNamedTUI(reader, "feature/codex", "codex", "/tmp/project", time.Now())
	})
	if err != nil || injections != 1 || !strings.Contains(stderr, "named feature/codex") {
		t.Fatalf("readback success: err=%v injections=%d stderr=%q", err, injections, stderr)
	}
}

func TestRunCoopNamedTUIReadbackFailureDoesNotReinject(t *testing.T) {
	oldInject := coopNamedTTYInject
	oldTimeout := coopNamedTUIReadbackTimeout
	oldSleep := coopNamedReadbackSleep
	t.Cleanup(func() {
		coopNamedTTYInject = oldInject
		coopNamedTUIReadbackTimeout = oldTimeout
		coopNamedReadbackSleep = oldSleep
	})
	reader := &fakeCoopNamedStoreReader{candidate: coopNamedStoreCandidate{}}
	injections := 0
	coopNamedTTYInject = func(string, string) error {
		injections++
		return nil
	}
	coopNamedTUIReadbackTimeout = time.Millisecond
	coopNamedReadbackSleep = time.Sleep
	_, stderr, err := captureEnvOutput(t, func() error {
		return runCoopNamedTUI(reader, "feature/codex", "codex", "/tmp/project", time.Now())
	})
	if err != nil || injections != 1 || reader.readCalls == 0 {
		t.Fatalf("readback failure execution: err=%v injections=%d reads=%d", err, injections, reader.readCalls)
	}
	if !strings.Contains(stderr, "enter \"/rename feature/codex\" manually") {
		t.Fatalf("manual reminder = %q", stderr)
	}
}

func TestRunCoopNamedTUIUnknownStoreSkipsInjection(t *testing.T) {
	oldInject := coopNamedTTYInject
	t.Cleanup(func() { coopNamedTTYInject = oldInject })
	injections := 0
	coopNamedTTYInject = func(string, string) error {
		injections++
		return nil
	}
	reader := &fakeCoopNamedStoreReader{locateErr: errors.New("store unavailable")}
	_, stderr, err := captureEnvOutput(t, func() error {
		return runCoopNamedTUI(reader, "feature/codex", "codex", "/tmp/project", time.Now())
	})
	if err != nil || injections != 0 || !strings.Contains(stderr, "store unavailable") {
		t.Fatalf("unknown store: err=%v injections=%d stderr=%q", err, injections, stderr)
	}
}

func TestRunCoopNamedInjectMissingSQLiteSkipsInjection(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	oldInject := coopNamedTTYInject
	t.Cleanup(func() { coopNamedTTYInject = oldInject })
	injections := 0
	coopNamedTTYInject = func(string, string) error {
		injections++
		return nil
	}
	reader := codexNamedStoreReader{}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, err := captureEnvOutput(t, func() error {
		return runCoopNamedTUI(&reader, "feature/codex", "codex", cwd, time.Now())
	})
	if err != nil || injections != 0 || !strings.Contains(stderr, "manually") {
		t.Fatalf("missing sqlite3: err=%v injections=%d stderr=%q", err, injections, stderr)
	}
}

func TestCodexNamedStoreReaderLive(t *testing.T) {
	if os.Getenv("AMQ_CODEX_LIVE") != "1" {
		t.Skip("set AMQ_CODEX_LIVE=1 to run the scratch Codex session proof")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex is unavailable: %v", err)
	}
	home := t.TempDir()
	cwd := t.TempDir()
	start := time.Now()
	cmd := exec.Command(codex, "exec", "--skip-git-repo-check", "--json", "Reply with exactly OK")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scratch Codex session: %v\n%s", err, output)
	}
	candidate, err := (codexNamedStoreReader{}).locate(cwd, start)
	if err != nil {
		t.Fatalf("read scratch Codex store: %v", err)
	}
	if _, err := (codexNamedStoreReader{}).readName(candidate); err != nil {
		t.Fatalf("read scratch Codex name: %v", err)
	}
}

func TestCursorNamedStoreReaderLive(t *testing.T) {
	if os.Getenv("AMQ_CURSOR_LIVE") != "1" {
		t.Skip("set AMQ_CURSOR_LIVE=1 to run the scratch Cursor session proof")
	}
	agent, err := exec.LookPath("agent")
	if err != nil {
		t.Skipf("Cursor agent is unavailable: %v", err)
	}
	home := t.TempDir()
	cwd := t.TempDir()
	start := time.Now()
	cmd := exec.Command(agent, "--print", "Reply with exactly OK")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "HOME="+home)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scratch Cursor session: %v\n%s", err, output)
	}
	candidate, err := (cursorNamedStoreReader{}).locate(cwd, start)
	if err != nil {
		t.Fatalf("read scratch Cursor store: %v", err)
	}
	if _, err := (cursorNamedStoreReader{}).readName(candidate); err != nil {
		t.Fatalf("read scratch Cursor name: %v", err)
	}
}
