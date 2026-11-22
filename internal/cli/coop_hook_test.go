package cli

// Purpose: guards the co-op hooks that amq coop install-hooks writes. The
// preamble test breaks if a participant's SessionStart misses its handle,
// session or unread count, or prints anything but the hook JSON. The stop
// tests break if new mail does not block once, a repeat in the chain blocks
// again, or a live wake's doorbell mail blocks. The installer test breaks if
// an entry loses its participant gate or matcher, if the waiter entries are
// missing or replace the others, or if old project entries that run the
// co-op scripts go unreported. Its Codex part breaks if the Codex wait hook
// is not written to $CODEX_HOME/hooks.json as an async entry on SessionStart
// and Stop behind the gate on the Codex identity store.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
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

func TestCoopSessionStartAttendedPreamble(t *testing.T) {
	clearSendMailboxTestEnv(t)
	dir := t.TempDir()
	base := filepath.Join(dir, ".agent-mail")
	root := filepath.Join(base, "collab")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envMe, "claude")
	t.Setenv(envRoot, root)
	setAttendedForTest(t, root, "claude")

	stdout, stderr := captureOutput(t, func() error { return writeCoopSessionStart(os.Stdout, time.Now()) })
	if stderr != "" {
		t.Fatalf("stderr = %q, want none", stderr)
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
	want := "This seat is attended: no doorbell is typed and no hook wakes it."
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, want) {
		t.Fatalf("attended preamble = %q, want it to contain %q", got.HookSpecificOutput.AdditionalContext, want)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "A wake rings this terminal") {
		t.Fatalf("attended preamble = %q, want no typed-doorbell case", got.HookSpecificOutput.AdditionalContext)
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
	// with no AM_ME and no binding file for its Claude session or its chat.
	store := `[ -e '` + filepath.Join(home, ".amq", "claude-sessions", "sessions") + `/'"${`
	gate := coopHookMarker + `[ -n "${AM_ME-}" ] || ` + store + `CLAUDE_CODE_SESSION_ID-}".json ] || ` + store + `CLAUDE_CODE_HOST_SESSION_ID-}".json ] || exit 0; `
	amq := "'" + mustExecutable(t) + "' --no-update-check coop hook "
	for _, want := range []struct {
		event, matcher, command string
		rewake                  bool
	}{
		{"SessionStart", coopSessionStartSources, gate + "exec " + amq + "session-start", false},
		{"Stop", "", gate + amq + "stop || true", false},
		{"UserPromptSubmit", "", gate + amq + "prompt || true", false},
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
					(want.rewake && group.Hooks[0].Timeout != coopWaitTimeout) ||
					(!want.rewake && group.Hooks[0].Timeout != 2) {
					t.Fatalf("%s entry = %+v, want matcher %q asyncRewake %v", want.event, group, want.matcher, want.rewake)
				}
			}
		}
		wantGroups := 2
		if want.event == "UserPromptSubmit" {
			wantGroups = 1
		}
		if found != 1 || len(settings.Hooks[want.event]) != wantGroups {
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

	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if _, err := captureStdout(t, func() error { return runCoopInstallHooks([]string{"--harness", "codex"}) }); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var codex struct {
		Hooks map[string][]struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
				Async   bool   `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &codex); err != nil {
		t.Fatal(err)
	}
	wait := coopHookMarker + `[ -n "${AM_ME-}" ] || [ -d '` + filepath.Join(home, ".amq", "codex-threads", "threads") + `' ] || exit 0; ` +
		amq + "codex-wait || true"
	for _, event := range []string{"SessionStart", "Stop"} {
		groups := codex.Hooks[event]
		if len(groups) != 1 || groups[0].Matcher != nil || len(groups[0].Hooks) != 1 ||
			groups[0].Hooks[0].Command != wait || !groups[0].Hooks[0].Async || groups[0].Hooks[0].Timeout != coopWaitTimeout {
			t.Fatalf("Codex %s = %+v, want one async entry running %q", event, groups, wait)
		}
	}
	prompt := strings.Replace(wait, "codex-wait", "codex-prompt", 1)
	if groups := codex.Hooks["UserPromptSubmit"]; len(codex.Hooks) != 3 || len(groups) != 1 || groups[0].Matcher != nil || len(groups[0].Hooks) != 1 ||
		groups[0].Hooks[0].Command != prompt || groups[0].Hooks[0].Async || groups[0].Hooks[0].Timeout != 2 {
		t.Fatalf("Codex UserPromptSubmit = %+v of %d events, want one entry running %q", groups, len(codex.Hooks), prompt)
	}
	if _, err := captureStdout(t, func() error { return runCoopInstallHooks([]string{"--harness", "codex"}) }); err != nil {
		t.Fatal(err)
	}
	if again, err := os.ReadFile(filepath.Join(codexHome, "hooks.json")); err != nil || !bytes.Equal(again, raw) {
		t.Fatalf("second Codex install changed the hooks (%v):\n%s", err, again)
	}
	if _, err := captureStdout(t, func() error { return runCoopUninstallHooks([]string{"--harness", "codex"}) }); err != nil {
		t.Fatal(err)
	}
	if left, err := os.ReadFile(filepath.Join(codexHome, "hooks.json")); err != nil || bytes.Contains(left, []byte(coopHookMarker)) {
		t.Fatalf("Codex uninstall left co-op entries (%v):\n%s", err, left)
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

func TestCoopPromptAddsUnreadContextOnlyForAHumanPrompt(t *testing.T) {
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

	var out bytes.Buffer
	if err := writeCoopPrompt(strings.NewReader(`{"prompt":"hello"}`), &out); err != nil {
		t.Fatal(err)
	}
	var response struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.HookSpecificOutput; got.HookEventName != "UserPromptSubmit" ||
		!strings.Contains(got.AdditionalContext, "1 unread AMQ message") || !strings.Contains(got.AdditionalContext, "session=collab") {
		t.Fatalf("prompt response = %+v", got)
	}
	out.Reset()
	// review of agent-message-queue-4tid: a long prompt lost the notice
	longPrompt := fmt.Sprintf(`{"prompt":%q}`, strings.Repeat("a", 1<<20))
	if err := writeCoopPrompt(strings.NewReader(longPrompt), &out); err != nil || !strings.Contains(out.String(), `"hookEventName":"UserPromptSubmit"`) {
		t.Fatalf("long prompt: output %q, error %v", out.String(), err)
	}
	out.Reset()
	if err := writeCoopPrompt(strings.NewReader(`{"prompt":"`+harness.Doorbell+`"}`), &out); err != nil || out.Len() != 0 {
		t.Fatalf("own doorbell: output %q, error %v", out.String(), err)
	}
	runDrainJSON(t, root, "claude", 100, false)
	out.Reset()
	if err := writeCoopPrompt(strings.NewReader(`{"prompt":"hello"}`), &out); err != nil || out.Len() != 0 {
		t.Fatalf("empty inbox: output %q, error %v", out.String(), err)
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

// An attended seat has no baseline cut: the attended wake
// types nothing, so mail created after its baseline would never be
// reported anywhere else. The decide function takes no baseline, so the
// unit case is that mail after a live wake's baseline is reported.
func sendCodexToClaudeForAttendedTest(t *testing.T, root string) {
	t.Helper()
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "codex", "--to", "claude", "--body", "hi"})
	}); err != nil {
		t.Fatal(err)
	}
}

// setAttendedForTest writes the agent's settings file with attended on.
func setAttendedForTest(t *testing.T, root, me string) {
	t.Helper()
	path := filepath.Join(fsq.AgentBase(root, me), wakeSettingsFileName)
	raw, err := json.Marshal(map[string]any{"schema": 1, "settings": map[string]bool{"attended": true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// An attended seat reports pending mail at a real turn end through
// writeCoopStop: the notice shows, no block is returned, a repeat for the
// same mail prints nothing, and a second message gets exactly one more
// notice. No live wake exists here, so there is no baseline to cut with —
// and the attended path never cuts by baseline anyway.
func TestCoopStopAttendedReportsPendingMailWithoutAWake(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	root := filepath.Join(base, "collab")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	sendCodexToClaudeForAttendedTest(t, root)
	t.Setenv(envMe, "claude")
	t.Setenv(envRoot, root)
	t.Setenv(envBaseRoot, base)
	t.Setenv(envSession, "collab")
	setAttendedForTest(t, root, "claude")

	var out bytes.Buffer
	stop := func() string {
		t.Helper()
		out.Reset()
		if err := writeCoopStop(strings.NewReader(`{"stop_hook_active":false}`), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first := stop()
	if !strings.Contains(first, "unread message(s)") {
		t.Fatalf("attended stop = %q, want the unread notice", first)
	}
	if strings.Contains(first, `"decision":"block"`) {
		t.Fatalf("attended stop = %q, want no block", first)
	}
	// A repeat for the same mail prints nothing: NoticedIDs already covers it.
	if got := stop(); got != "" {
		t.Fatalf("repeat attended stop = %q, want no output", got)
	}
	// A second message prints one more notice, once.
	sendCodexToClaudeForAttendedTest(t, root)
	second := stop()
	if !strings.Contains(second, "2 unread message(s)") {
		t.Fatalf("second attended stop = %q, want the notice for both messages", second)
	}
	if got := stop(); got != "" {
		t.Fatalf("repeat after both noticed = %q, want no output", got)
	}
}

// A drained message leaves the attended Stop state at the next turn end:
// the state holds exactly the ids still pending, so it does not grow with
// every message ever reported. The previous ids are what keeps the notice
// from repeating, not what the state keeps.
func TestCoopStopAttendedStateHoldsOnlyPendingIDs(t *testing.T) {
	old := coopStopState{Schema: 1, NoticedIDs: []string{"already-drained"}}
	if got := decideCoopStopAttended(nil, old); len(got.NoticedIDs) != 0 {
		t.Fatalf("empty inbox still records %v, want an empty state", got.NoticedIDs)
	}
	kept := coopStopMessage{ID: "kept", Created: "2026-11-22T12:00:00Z"}
	got := decideCoopStopAttended([]coopStopMessage{kept}, old)
	if !slices.Equal(got.NoticedIDs, []string{"kept"}) {
		t.Fatalf("drained mail kept in state: %v, want [kept]", got.NoticedIDs)
	}
	if !slices.Equal(got.BlockedIDs, old.BlockedIDs) {
		t.Fatalf("attended seat changed BlockedIDs: %v, want it untouched", got.BlockedIDs)
	}
	// An id an earlier unattended block reported stays reported: an attended
	// seat that keeps it pending reports it again only as a systemMessage
	// count, never as a second block.
	blockedOld := coopStopState{Schema: 1, BlockedIDs: []string{"kept"}}
	blockedGot := decideCoopStopAttended([]coopStopMessage{kept}, blockedOld)
	if fresh := freshAttendedIDs([]coopStopMessage{kept}, blockedGot.NoticedIDs, blockedGot.BlockedIDs); len(fresh) != 0 {
		t.Fatalf("blocked id reported again by freshAttendedIDs: %v, want none", fresh)
	}
	// Drained mail leaves the unattended state too: decideCoopStop prunes
	// NoticedIDs to ids still pending at the same turn end.
	drained := coopStopState{Schema: 1, NoticedIDs: []string{"gone", "kept"}}
	next, out := decideCoopStop([]coopStopMessage{kept}, nil, drained, false, "root=r session=s")
	if !slices.Equal(next.NoticedIDs, []string{"kept"}) {
		t.Fatalf("unattended state keeps drained mail: %v, want [kept]", next.NoticedIDs)
	}
	if out == nil {
		t.Fatal("unattended stop with fresh mail blocked, want a block decision")
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
