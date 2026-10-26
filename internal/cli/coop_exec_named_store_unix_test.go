//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
	coopNamedTTYInject = func(string, string, func() bool) error {
		injections++
		return nil
	}
	_, stderr, err := captureEnvOutput(t, func() error {
		return runCoopNamedTUI(reader, "feature/codex", "codex", "/tmp/project", time.Now(), nil)
	})
	if err != nil || injections != 0 || stderr != "" {
		t.Fatalf("named store skip: err=%v injections=%d stderr=%q", err, injections, stderr)
	}

	reader = &fakeCoopNamedStoreReader{candidate: coopNamedStoreCandidate{}, names: []string{"", "feature/codex"}}
	coopNamedTUIReadbackTimeout = time.Second
	coopNamedReadbackSleep = func(time.Duration) {}
	_, stderr, err = captureEnvOutput(t, func() error {
		return runCoopNamedTUI(reader, "feature/codex", "codex", "/tmp/project", time.Now(), nil)
	})
	if err != nil || injections != 1 || !strings.Contains(stderr, "named feature/codex") {
		t.Fatalf("readback success: err=%v injections=%d stderr=%q", err, injections, stderr)
	}
}

// W1 review (Pro finding 5): the Cursor namer checked the dialog marker
// once, then typed its command and, after the paste delay, Enter, which a
// dialog opened meanwhile would take. Each write and the Enter check again.
func TestCoopNamedSlashCommandStopsWhenADialogOpens(t *testing.T) {
	oldReady, oldInject := coopNamedTTYReady, tiocstiInject
	t.Cleanup(func() { coopNamedTTYReady, tiocstiInject = oldReady, oldInject })
	stubRawInjectSleep(t)
	coopNamedTTYReady = func() bool { return true }
	var written []string
	tiocstiInject = func(text string) error {
		written = append(written, text)
		return nil
	}
	checks := 0
	inputFree := func() bool {
		checks++
		return checks == 1
	}
	err := injectCoopNamedSlashCommand("s1/cur", "cursor-agent", inputFree)
	if err == nil || len(written) != 1 || written[0] != "/rename s1/cur" {
		t.Fatalf("err = %v, written = %q; want the command typed and no Enter after the dialog opened", err, written)
	}
}
