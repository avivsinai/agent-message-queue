package cli

// Purpose: guards the co-op hooks that amq coop install-hooks writes. The
// preamble test breaks if a participant's SessionStart misses its handle,
// session or unread count, or prints anything but the hook JSON. The
// installer test breaks if the entry loses its AM_ME gate or matcher, or if
// old project entries that run the co-op scripts go unreported.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoopHookSessionStartPrintsThePreamble(t *testing.T) {
	clearSendMailboxTestEnv(t)
	dir := t.TempDir()
	base := filepath.Join(dir, ".agent-mail")
	root := filepath.Join(base, "collab")
	queue := filepath.Join(dir, "queue")
	for _, r := range []string{root, filepath.Join(base, "other"), queue} {
		if err := runInit([]string{"--root", r, "--agents", "claude,codex"}); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error {
			return runSend([]string{"--root", r, "--me", "codex", "--to", "claude", "--body", "hi"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".amqrc"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name             string
		root, base, sesh string
		want             []string // empty: the hook prints nothing
	}{
		{"pinned session", root, base, "collab", []string{"me=claude session=collab peers=codex", "Inbox: 1 unread."}},
		// Regression (#1171 review): AM_ROOT names one session and the pin
		// another; amq env refuses this, so the hook must not pick a side.
		{"pin names another session", filepath.Join(base, "other"), base, "collab", nil},
		// Regression (#1171 review): the session came from .amqrc discovery,
		// so a malformed .amqrc near AM_ROOT printed a warning.
		{"no pin, malformed .amqrc nearby", queue, "", "", []string{"me=claude peers=codex", "Inbox: 1 unread."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envMe, "claude")
			t.Setenv(envRoot, tc.root)
			setOptionalEnv(t, envBaseRoot, tc.base, tc.base != "")
			setOptionalEnv(t, envSession, tc.sesh, tc.sesh != "")
			stdout, stderr := captureOutput(t, func() error { return writeCoopSessionStart(os.Stdout, time.Now()) })
			if stderr != "" {
				t.Fatalf("stderr = %q, want none", stderr)
			}
			if tc.want == nil {
				if stdout != "" {
					t.Fatalf("stdout = %q, want none", stdout)
				}
				return
			}
			var got struct {
				HookSpecificOutput struct {
					HookEventName     string `json:"hookEventName"`
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("output %q: %v", stdout, err)
			}
			for _, want := range tc.want {
				if got.HookSpecificOutput.HookEventName != "SessionStart" || !strings.Contains(got.HookSpecificOutput.AdditionalContext, want) {
					t.Fatalf("preamble = %+v, want SessionStart with %q", got.HookSpecificOutput, want)
				}
			}
		})
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
