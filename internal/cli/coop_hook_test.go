package cli

// Purpose: guards the co-op hooks that amq coop install-hooks writes. The
// preamble test breaks if a participant's SessionStart misses its handle,
// session or unread count, or prints anything but the hook JSON. The
// installer test breaks if the entry loses its AM_ME gate or matcher, or if
// old project entries that run the co-op scripts go unreported.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoopHookSessionStartPrintsThePreamble(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), ".agent-mail", "collab")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "codex", "--to", "claude", "--body", "hi"})
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envMe, "claude")
	t.Setenv(envRoot, root)

	var out bytes.Buffer
	if err := writeCoopSessionStart(&out, time.Now()); err != nil {
		t.Fatal(err)
	}
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	context := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"me=claude session=collab peers=codex", "Inbox: 1 unread."} {
		if got.HookSpecificOutput.HookEventName != "SessionStart" || !strings.Contains(context, want) {
			t.Fatalf("preamble = %+v, want SessionStart with %q", got.HookSpecificOutput, want)
		}
	}
}

func TestCoopInstallHooksWritesTheGatedEntryAndListsOldOnes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"scripts/claude-session-start.sh"}]}]}}`
	if err := os.WriteFile(filepath.Join(project, ".claude", "settings.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	out, err := captureStdout(t, func() error { return runCoopInstallHooks(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "claude-session-start.sh") {
		t.Fatalf("install output does not list the old project entry:\n%s", out)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	groups := settings.Hooks["SessionStart"]
	if len(groups) != 1 || groups[0].Matcher != coopSessionStartSources || len(groups[0].Hooks) != 1 ||
		!strings.HasPrefix(groups[0].Hooks[0].Command, coopHookMarker+`[ -z "${AM_ME-}" ] || exec `) {
		t.Fatalf("SessionStart = %+v, want one gated co-op entry", groups)
	}
}
