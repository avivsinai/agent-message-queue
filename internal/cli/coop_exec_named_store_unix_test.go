//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

func TestCursorNamedStoreReaderFiltersCWDAndWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	createdAt := strconv.FormatInt(start.UnixMilli(), 10)
	metaPath := filepath.Join(home, ".cursor", "chats", "workspace", "chat", "meta.json")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte(`{"title":"","createdAtMs":`+createdAt+`,"cwd":"`+cwd+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := (cursorNamedStoreReader{}).locate(cwd, start)
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
	if err := os.WriteFile(other, []byte(`{"title":"other","createdAtMs":`+createdAt+`,"cwd":"`+cwd+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (cursorNamedStoreReader{}).locate(cwd, start); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("multiple Cursor candidates error = %v", err)
	}
}

func TestCoopNamedStoreReaderRecognizesCursorAliases(t *testing.T) {
	current, currentOK := coopNamedStoreReaderFor("agent")
	if !currentOK {
		t.Fatal("current Cursor executable does not use the Cursor named store reader")
	}
	if _, ok := current.(cursorNamedStoreReader); !ok {
		t.Fatal("current Cursor executable returned the wrong named store reader")
	}
	legacy, legacyOK := coopNamedStoreReaderFor("cursor-agent")
	if !legacyOK {
		t.Fatal("legacy Cursor executable does not use the Cursor named store reader")
	}
	if _, ok := legacy.(cursorNamedStoreReader); !ok {
		t.Fatal("legacy Cursor executable returned the wrong named store reader")
	}
}

func TestCursorNamedStoreReaderAcceptsClockSlackButFiltersOlderChats(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	withinCreated := start.Add(-time.Second)
	oldCreated := start.Add(-3 * time.Second)
	within := filepath.Join(home, ".cursor", "chats", "workspace", "within", "meta.json")
	old := filepath.Join(home, ".cursor", "chats", "workspace", "old", "meta.json")
	for _, test := range []struct {
		path      string
		createdAt time.Time
	}{
		{path: within, createdAt: withinCreated},
		{path: old, createdAt: oldCreated},
	} {
		path := test.path
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"title":"","createdAtMs":`+strconv.FormatInt(test.createdAt.UnixMilli(), 10)+`,"cwd":"`+cwd+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withinTime := time.Now()
	oldTime := withinTime
	if err := os.Chtimes(within, withinTime, withinTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	candidate, err := (cursorNamedStoreReader{}).locate(cwd, start)
	if err != nil {
		t.Fatalf("locate Cursor candidate with clock slack: %v", err)
	}
	if candidate.storePath != within {
		t.Fatalf("candidate = %#v, want chat within clock slack", candidate)
	}
}

func TestCursorNamedStoreReaderWithoutCreationEvidenceSkips(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux has no supported Cursor chat birth-time fallback")
	}
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
	oldInject := coopNamedTTYInject
	t.Cleanup(func() { coopNamedTTYInject = oldInject })
	injections := 0
	coopNamedTTYInject = func(string, string) error {
		injections++
		return nil
	}
	_, stderr, err := captureEnvOutput(t, func() error {
		reader := cursorNamedStoreReader{}
		return runCoopNamedTUI(&reader, "feature/agent", "agent", cwd, time.Now())
	})
	if err != nil || injections != 0 || !strings.Contains(stderr, "manually") {
		t.Fatalf("missing Cursor creation evidence: err=%v injections=%d stderr=%q", err, injections, stderr)
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
