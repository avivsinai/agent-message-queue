package cli

// Purpose: guards the co-op hooks that amq coop install-hooks writes. The
// preamble test breaks if a participant's SessionStart misses its handle,
// session or unread count, or prints anything but the hook JSON. The stop
// tests break if new mail does not block once, a repeat in the chain blocks
// again, or a live wake's doorbell mail blocks. The installer test breaks if
// an entry loses its participant gate or matcher, if the waiter entries are
// missing or replace the others, or if old project entries that run the
// co-op scripts go unreported.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
				Command     string `json:"command"`
				Timeout     int    `json:"timeout"`
				AsyncRewake bool   `json:"asyncRewake"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	// Every entry starts no amq in a session that does not take part: one
	// with no AM_ME and no binding file for its Claude session.
	gate := coopHookMarker + `[ -n "${AM_ME-}" ] || [ -e '` + filepath.Join(home, ".amq", "claude-sessions", "sessions") + `/'"${CLAUDE_CODE_SESSION_ID-}".json ] || exit 0; `
	amq := "'" + mustExecutable(t) + "' --no-update-check coop hook "
	for _, want := range []struct {
		event, matcher, command string
		rewake                  bool
	}{
		{"SessionStart", coopSessionStartSources, gate + "exec " + amq + "session-start", false},
		{"Stop", "", gate + amq + "stop || true", false},
		// The waiter rings with exit 10; only that becomes the 2 that wakes
		// the session, so a usage error of an older amq never rings.
		{"SessionStart", coopSessionStartSources, gate + amq + "wait; [ $? -ne 10 ] || exit 2", true},
		{"Stop", "", gate + amq + "wait; [ $? -ne 10 ] || exit 2", true},
	} {
		found := 0
		for _, group := range settings.Hooks[want.event] {
			if len(group.Hooks) == 1 && group.Hooks[0].Command == want.command {
				found++
				if group.Matcher != want.matcher || group.Hooks[0].AsyncRewake != want.rewake ||
					(want.rewake && group.Hooks[0].Timeout != coopWaitTimeout) {
					t.Fatalf("%s entry = %+v, want matcher %q asyncRewake %v", want.event, group, want.matcher, want.rewake)
				}
			}
		}
		if found != 1 || len(settings.Hooks[want.event]) != 2 {
			t.Fatalf("%s = %+v, want the co-op entry and the waiter entry, with %q once", want.event, settings.Hooks[want.event], want.command)
		}
	}
	// A second install converges: the waiter does not replace the guard.
	if _, err := captureStdout(t, func() error { return runCoopInstallHooks(nil) }); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil || !bytes.Equal(again, raw) {
		t.Fatalf("second install changed the settings (%v):\n%s", err, again)
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestCoopHookStopBlocksOnceForNewMail(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	root := filepath.Join(base, "collab")
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
	t.Setenv(envBaseRoot, base)
	t.Setenv(envSession, "collab")

	stop := func(active string) string {
		t.Helper()
		var out bytes.Buffer
		if err := writeCoopStop(strings.NewReader(`{"stop_hook_active":`+active+`}`), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := stop("false"); !strings.Contains(got, `"decision":"block"`) || !strings.Contains(got, "session=collab") {
		t.Fatalf("first stop = %q, want a block naming the session", got)
	}
	if got := stop("true"); got != "" {
		t.Fatalf("repeat stop = %q, want no output", got)
	}
	info, err := os.Stat(filepath.Join(root, "agents", "claude", coopStopStateFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file = %v, %v; want mode 0600", info, err)
	}
}

// Regression (#1152): under a live wake every turn end blocked for mail the
// doorbell was about to deliver. Only mail created at or before the wake's
// baseline_at, the cohort it never rings for, may block.
func TestCoopStopUnderALiveWakeCountsOnlyBaselineMail(t *testing.T) {
	baseline := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	before := coopStopMessage{ID: "before", Created: baseline.Add(-time.Second).Format(time.RFC3339Nano)}
	after := coopStopMessage{ID: "after", Created: baseline.Add(time.Second).Format(time.RFC3339Nano)}
	fresh := coopStopState{Schema: 1, BlockedIDs: []string{}}

	if _, out := decideCoopStop([]coopStopMessage{after}, &baseline, fresh, false, "root=r session=s"); out != nil {
		t.Fatalf("mail after the baseline: %v, want no block", out)
	}
	next, out := decideCoopStop([]coopStopMessage{before, after}, &baseline, fresh, false, "root=r session=s")
	if out["decision"] != "block" || !slices.Equal(next.BlockedIDs, []string{"before"}) {
		t.Fatalf("mail at the baseline: %v blocked %v, want a block for before only", out, next.BlockedIDs)
	}
}

func TestCoopStopAllowsTheStopAfterFiveBlocksInAChain(t *testing.T) {
	old := coopStopState{Schema: 1, BlockedIDs: []string{"a"}, ChainBlocks: coopStopBlockLimit}
	next, out := decideCoopStop([]coopStopMessage{{ID: "a"}, {ID: "b"}}, nil, old, true, "root=r session=s")
	if !strings.Contains(out["systemMessage"], "budget exhausted") || next.ChainBlocks != 0 {
		t.Fatalf("sixth block: %v chain %d, want a notice and a reset chain", out, next.ChainBlocks)
	}
}

// The preamble's unread count skips a corrupt message, as amq list --new
// does (audit 2026-11-10).
func TestCoopHookUnreadSkipsACorruptMessage(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "codex", "--to", "claude", "--body", "hi"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "claude", "inbox", "new", "corrupt.md"), []byte("not a message"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := coopHookUnread(root, "claude"); got != 1 {
		t.Fatalf("unread = %d, want 1", got)
	}
}
