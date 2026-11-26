//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
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
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
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

// One corrupt meta.json for another chat must not stop Cursor naming for this
// cwd's chat (audit 2026-11-10).
func TestCursorNamedStoreReaderSkipsCorruptMeta(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	good := filepath.Join(home, ".cursor", "chats", "workspace", "good", "meta.json")
	bad := filepath.Join(home, ".cursor", "chats", "workspace", "bad", "meta.json")
	for path, body := range map[string]string{
		good: `{"title":"t","createdAtMs":` + strconv.FormatInt(start.UnixMilli(), 10) + `,"cwd":"` + cwd + `"}`,
		bad:  `{"title":`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	candidate, err := (cursorNamedStoreReader{}).locate(cwd, start)
	if err != nil || candidate.storePath != good {
		t.Fatalf("locate = %#v, %v; want the valid chat", candidate, err)
	}
}

func TestCursorNamedStoreReaderUsesCursorConfigPrecedence(t *testing.T) {
	home, config, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	cwd, start := t.TempDir(), time.Now()
	homeChat := writeNamedStoreMeta(t, filepath.Join(home, ".cursor"), "h", cwd, "home", start)
	configChat := writeNamedStoreMeta(t, config, "c", cwd, "config", start)
	xdgChat := writeNamedStoreMeta(t, filepath.Join(xdg, "cursor"), "x", cwd, "xdg", start)
	for _, tc := range []struct{ name, config, xdg, want string }{
		{name: "explicit", config: config, xdg: xdg, want: configChat},
		{name: "xdg", config: " ", xdg: xdg, want: xdgChat},
		{name: "home", want: homeChat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CURSOR_CONFIG_DIR", tc.config)
			t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			got, err := (cursorNamedStoreReader{}).locate(cwd, start)
			if err != nil || got.storePath != tc.want {
				t.Fatalf("locate = %q, %v; want %q", got.storePath, err, tc.want)
			}
		})
	}
}

func writeNamedStoreMeta(t *testing.T, config, id, cwd, title string, createdAt time.Time) string {
	t.Helper()
	path := filepath.Join(config, "chats", "workspace", id, "meta.json")
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "createdAtMs": createdAt.UnixMilli(), "cwd": cwd,
		"title": title, "hasConversation": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWaitCoopNamedStoreCandidateWaitsForExactNewMetadata(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", config)
	cwd, start := t.TempDir(), time.Now()
	// Neither an older chat in this workspace nor a fresh chat elsewhere
	// authorizes typing. A fresh sidecar in this workspace arrives later.
	writeNamedStoreMeta(t, config, "old", cwd, "", start.Add(-time.Minute))
	writeNamedStoreMeta(t, config, "other", t.TempDir(), "", start)
	oldSleep := coopNamedLocateSleep
	t.Cleanup(func() { coopNamedLocateSleep = oldSleep })
	var want string
	coopNamedLocateSleep = func(time.Duration) {
		if want != "" {
			t.Fatal("did not accept the exact newly written chat")
		}
		want = writeNamedStoreMeta(t, config, "target", cwd, "", start)
	}
	got, err := waitCoopNamedStoreCandidate(cursorNamedStoreReader{}, cwd, start)
	if err != nil || want == "" || got.storePath != want {
		t.Fatalf("candidate = %q, %v; want late metadata %q", got.storePath, err, want)
	}
}

func TestWaitCoopNamedStoreCandidateDoesNotWaitOnAmbiguity(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", config)
	cwd, start := t.TempDir(), time.Now()
	writeNamedStoreMeta(t, config, "a", cwd, "", start)
	writeNamedStoreMeta(t, config, "b", cwd, "", start)
	oldSleep := coopNamedLocateSleep
	t.Cleanup(func() { coopNamedLocateSleep = oldSleep })
	coopNamedLocateSleep = func(time.Duration) { t.Fatal("waited after an ambiguous match") }
	if _, err := waitCoopNamedStoreCandidate(cursorNamedStoreReader{}, cwd, start); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("locate error = %v; want ambiguous", err)
	}
}

func TestWaitCoopNamedStoreCandidateStopsWhenTimeoutExpires(t *testing.T) {
	oldTimeout, oldSleep := coopNamedTUILocateTimeout, coopNamedLocateSleep
	t.Cleanup(func() { coopNamedTUILocateTimeout, coopNamedLocateSleep = oldTimeout, oldSleep })
	coopNamedTUILocateTimeout = 0
	coopNamedLocateSleep = func(time.Duration) { t.Fatal("waited after timeout") }
	reader := &fakeCoopNamedStoreReader{locateErr: errCursorNamedStoreAbsent}
	if _, err := waitCoopNamedStoreCandidate(reader, t.TempDir(), time.Now()); !errors.Is(err, errCursorNamedStoreAbsent) {
		t.Fatalf("locate error = %v; want store absent", err)
	}
}
