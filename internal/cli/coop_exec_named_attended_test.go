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

// writeCursorChatMetaForTest writes one Cursor chat store candidate into a
// fake HOME, with the given title and the test process's cwd.
func writeCursorChatMetaForTest(t *testing.T, home, title string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(home, ".cursor", "chats", "workspace", "chat", "meta.json")
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"title":"` + title + `","createdAtMs":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `,"cwd":"` + cwd + `"}`
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runCoopNamedInjectAttended(t *testing.T, root string) string {
	t.Helper()
	_, stderr, err := captureEnvOutput(t, func() error {
		return runCoopNamedInject([]string{
			"--name", "feature/x", "--binary", "cursor-agent",
			"--root", root, "--me", "claude",
			"--startup-delay", "0",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return stderr
}

// A naming attempt whose store candidate already carries the name does not
// reach the inject step at all: the readback confirms the name and no
// attended refusal appears.
func TestRunCoopNamedInjectOnAttendedSeatSkipsTheNamedCandidate(t *testing.T) {
	oldTimeout, oldSleep := coopNamedTUIReadbackTimeout, coopNamedReadbackSleep
	t.Cleanup(func() { coopNamedTUIReadbackTimeout, coopNamedReadbackSleep = oldTimeout, oldSleep })
	coopNamedTUIReadbackTimeout = time.Second
	coopNamedReadbackSleep = func(time.Duration) {}

	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	setAttendedForTest(t, root, "claude")

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	writeCursorChatMetaForTest(t, home, "feature/x")

	stderr := runCoopNamedInjectAttended(t, root)
	if strings.Contains(stderr, "the seat is attended") {
		t.Fatalf("stderr = %q, want no attended reminder for a named candidate", stderr)
	}
}

// A naming attempt on an attended seat whose candidate has no name reaches
// the inject step, and the inputFree closure refuses it: nothing is
// injected and the manual reminder is the only output.
func TestRunCoopNamedInjectOnAttendedSeatRefusesTheUnnamedCandidate(t *testing.T) {
	oldInject := coopNamedTTYInject
	t.Cleanup(func() { coopNamedTTYInject = oldInject })
	injections := 0
	coopNamedTTYInject = func(me, binaryBase string, inputFree func() bool) error {
		if inputFree != nil && !inputFree() {
			return nil
		}
		injections++
		return nil
	}

	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	setAttendedForTest(t, root, "claude")

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	writeCursorChatMetaForTest(t, home, "")

	stderr := runCoopNamedInjectAttended(t, root)
	if injections != 0 {
		t.Fatalf("attended seat injections = %d, want 0", injections)
	}
	if !strings.Contains(stderr, coopNamedTUIManualReminder("feature/x", "cursor-agent", "the seat is attended")) {
		t.Fatalf("stderr = %q, want the attended manual reminder", stderr)
	}
}

func TestRunCoopNamedInjectRefusesWritesIfOriginalParentExited(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "cursor"}); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", config)
	writeNamedStoreMeta(t, config, "target", cwd, "", time.Now())
	oldInject := coopNamedTTYInject
	t.Cleanup(func() { coopNamedTTYInject = oldInject })
	checked := false
	coopNamedTTYInject = func(_, _ string, inputFree func() bool) error {
		checked = true
		if inputFree() {
			t.Fatal("allowed a write after the launching process exited")
		}
		return errCoopNamedDialogOpen
	}
	_, _, err = captureEnvOutput(t, func() error {
		return runCoopNamedInject([]string{
			"--name", "s1/cursor", "--binary", "cursor-agent",
			"--root", root, "--me", "cursor", "--startup-delay", "0",
			"--parent-pid", strconv.Itoa(os.Getpid()),
		})
	})
	if err != nil || !checked {
		t.Fatalf("naming returned %v, checked input = %t", err, checked)
	}
}
