//go:build darwin || linux

package cli

// tvgv: these receiver and installation tests protect turn-end, person-only
// notification without a turn continuation, and preservation of the waiter.
// Their only public seams are the hook receivers and hook-file commands.
// Unmodified main has neither codex-stop nor its synchronous Stop entry.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/hookfile"
)

func TestCoopStopCodexReportsAnAttendedSeatOncePerNewMail(t *testing.T) {
	const thread = "019a1166-dd8e-77c0-bc3b-a4b7e24e91f1"
	joinCodexThread(t, thread)
	root := absPath(filepath.Join(defaultCoopRoot, defaultSessionName))
	send := func(body string) {
		t.Helper()
		if _, err := captureStdout(t, func() error {
			return runSend([]string{"--root", root, "--me", "claude", "--to", "codex", "--body", body})
		}); err != nil {
			t.Fatal(err)
		}
	}
	stop := func() string {
		t.Helper()
		defer func() {
			for _, key := range append([]string{"CODEX_THREAD_ID"}, identityEnvKeys...) {
				_ = os.Unsetenv(key)
			}
		}()
		var out bytes.Buffer
		input := fmt.Sprintf(`{"session_id":%q,"hook_event_name":"Stop"}`, thread)
		if err := runCoopStopCodex(strings.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	setAttendedForTest(t, root, "codex")
	send("first")
	if got, want := stop(), `{"systemMessage":"AMQ: 1 unread message(s). Run amq drain --include-body."}`+"\n"; got != want {
		t.Fatalf("first turn end = %q, want %q", got, want)
	}
	if got := stop(); got != "" {
		t.Fatalf("the same mail got %q, want no notice", got)
	}
	send("second")
	if got, want := stop(), `{"systemMessage":"AMQ: 2 unread message(s). Run amq drain --include-body."}`+"\n"; got != want {
		t.Fatalf("next turn end = %q, want %q", got, want)
	}
}

func TestCoopStopCodexSpeaksOnlyForFreshMailOnItsAttendedThread(t *testing.T) {
	const thread, unbound, invalid = "019a1166-dd8e-77c0-bc3b-a4b7e24e91f1", "019a1166-dd8e-77c0-bc3b-a4b7e24e91f2", "unbound"
	for _, tc := range []struct {
		name, sessionID   string
		attended, blocked bool
	}{
		{name: "not attended", sessionID: thread},
		{name: "no binding", sessionID: unbound, attended: true},
		{name: "bad thread id", sessionID: invalid, attended: true},
		{name: "already blocked by Claude guard", sessionID: thread, attended: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinCodexThread(t, thread)
			root := absPath(filepath.Join(defaultCoopRoot, defaultSessionName))
			if _, err := captureStdout(t, func() error {
				return runSend([]string{"--root", root, "--me", "claude", "--to", "codex", "--body", "mail"})
			}); err != nil {
				t.Fatal(err)
			}
			if tc.attended {
				setAttendedForTest(t, root, "codex")
			}
			statePath := filepath.Join(fsq.AgentBase(root, "codex"), coopStopStateFile)
			if tc.blocked {
				pending, err := coopStopPending(root, "codex")
				if err != nil || len(pending) != 1 {
					t.Fatalf("pending = %v, %v", pending, err)
				}
				old := coopStopState{Schema: 1, BlockedIDs: []string{pending[0].ID}}
				if err := writeCoopHookState(statePath, old); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range append([]string{"CODEX_THREAD_ID"}, identityEnvKeys...) {
				_ = os.Unsetenv(key)
			}
			var out bytes.Buffer
			if err := runCoopStopCodex(strings.NewReader(fmt.Sprintf(`{"session_id":%q,"hook_event_name":"Stop"}`, tc.sessionID)), &out); err != nil {
				t.Fatal(err)
			}
			// Pre-existing blocked state also stays quiet. Its preservation is
			// the shared helper's attended bookkeeping, not a second notice.
			if got := out.String(); got != "" {
				t.Fatalf("receiver printed %q, want nothing", got)
			} else if !tc.blocked {
				if _, err := os.Stat(statePath); !os.IsNotExist(err) {
					t.Fatalf("state write = %v, want none", err)
				}
			}
		})
	}
}

func TestCoopStopCodexSilencesNoticeOnInvalidRecordedState(t *testing.T) {
	const thread = "019a1166-dd8e-77c0-bc3b-a4b7e24e91f1"
	joinCodexThread(t, thread)
	root := absPath(filepath.Join(defaultCoopRoot, defaultSessionName))
	setAttendedForTest(t, root, "codex")
	statePath := filepath.Join(fsq.AgentBase(root, "codex"), coopStopStateFile)
	raw := []byte("not one JSON object")
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range append([]string{"CODEX_THREAD_ID"}, identityEnvKeys...) {
		_ = os.Unsetenv(key)
	}
	var out bytes.Buffer
	if err := runCoopStopCodex(strings.NewReader(fmt.Sprintf(`{"session_id":%q,"hook_event_name":"Stop"}`, thread)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("receiver counted invalid state as absent and printed %q", out.String())
	}
	if got, err := os.ReadFile(statePath); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("invalid state changed on a silent failure: %q, %v", got, err)
	}
}

func TestCoopStopCodexSilencesNoticeWhenItsStateCannotChange(t *testing.T) {
	const thread = "019a1166-dd8e-77c0-bc3b-a4b7e24e91f1"
	joinCodexThread(t, thread)
	root := absPath(filepath.Join(defaultCoopRoot, defaultSessionName))
	setAttendedForTest(t, root, "codex")
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "claude", "--to", "codex", "--body", "mail"})
	}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(fsq.AgentBase(root, "codex"), coopStopStateFile)
	sentinel := []byte("unwritable state sentinel")
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "sentinel"), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range append([]string{"CODEX_THREAD_ID"}, identityEnvKeys...) {
		_ = os.Unsetenv(key)
	}
	var out bytes.Buffer
	input := strings.NewReader(fmt.Sprintf(`{"session_id":%q,"hook_event_name":"Stop"}`, thread))
	for i := 0; i < 2; i++ {
		if _, err := input.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		if err := runCoopStopCodex(input, &out); err != nil {
			t.Fatal(err)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("state write failed but Codex printed %q", out.String())
	}
	if raw, err := os.ReadFile(filepath.Join(statePath, "sentinel")); err != nil || !bytes.Equal(raw, sentinel) {
		t.Fatalf("existing state changed: %q, %v", raw, err)
	}
}

func TestCodexCoopStopInstallsAlongsideTheWaiter(t *testing.T) {
	clearJoinTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "hooks.json")
	bin := mustExecutable(t)
	threads := codexidentity.ThreadsDir(home)
	original := []hookfile.CommandHook{
		{Event: "SessionStart", Marker: coopHookMarker, Command: codexCoopWaitCommand(bin, threads), Timeout: coopWaitTimeout, Async: true},
		{Event: "Stop", Marker: coopHookMarker, Command: codexCoopWaitCommand(bin, threads), Timeout: coopWaitTimeout, Async: true},
		{Event: "UserPromptSubmit", Marker: coopHookMarker, Command: codexCoopPromptCommand(bin, threads), Timeout: 2},
	}
	if _, err := hookfile.InstallCommandHooksAt(path, original); err != nil {
		t.Fatal(err)
	}
	install := func() {
		t.Helper()
		if _, err := captureStdout(t, func() error { return runCoopInstallHooks([]string{"--harness", "codex"}) }); err != nil {
			t.Fatal(err)
		}
	}
	install()
	gate := `[ -n "${AM_ME-}" ] || [ -d ` + shellQuote(threads) + ` ] || exit 0; ` + shellQuote(bin) + ` --no-update-check coop hook `
	want := []struct {
		event, command string
		timeout        int
		async          bool
	}{
		{event: "SessionStart", command: "codex-wait", timeout: coopWaitTimeout, async: true},
		{event: "Stop", command: "codex-wait", timeout: coopWaitTimeout, async: true},
		{event: "Stop", command: "codex-stop", timeout: 2},
		{event: "UserPromptSubmit", command: "codex-prompt", timeout: 2},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
				Async   bool   `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Hooks) != 3 {
		t.Fatalf("got %d events, want 3: %s", len(file.Hooks), raw)
	}
	for _, expected := range want {
		command := gate + expected.command + ` || true`
		marker := coopHookMarker
		if expected.command == "codex-stop" {
			marker = codexStopHookMarker
		}
		match := 0
		for _, group := range file.Hooks[expected.event] {
			for _, hook := range group.Hooks {
				if hook.Command == marker+command {
					match++
					if hook.Async != expected.async || hook.Timeout != expected.timeout {
						t.Errorf("%s hook has async=%v timeout=%d, want async=%v timeout=%d", expected.command, hook.Async, hook.Timeout, expected.async, expected.timeout)
					}
				}
			}
		}
		if match != 1 {
			t.Errorf("%s %s full want %q: matched %d entries, want exactly one: %s", expected.event, expected.command, marker+command, match, raw)
		}
	}
	install()
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("second install changed the file (%v):\n%s", err, after)
	}
	if _, err := captureStdout(t, func() error { return runCoopUninstallHooks([]string{"--harness", "codex"}) }); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || bytes.Contains(after, []byte(coopHookMarker)) || bytes.Contains(after, []byte(codexStopHookMarker)) {
		t.Fatalf("uninstall left an entry (%v):\n%s", err, after)
	}
}
