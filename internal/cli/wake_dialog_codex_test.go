//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// The Codex approval guard, happy path. The installed Codex commands run
// under /bin/sh and zsh (Codex runs hooks under the user's $SHELL) with a
// fake binary: with AM_ME the open starts it, and with neither AM_ME nor the
// codexidentity store nothing starts. Then the real receiver opens the
// marker from the PermissionRequest Codex 0.163 sent, and the turn's Stop
// removes it.
func TestCodexDialogHookHoldsUntilTheTurnEnds(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "amq")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch \"$RAN\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(fsq.AgentBase(root, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	shells := []string{"/bin/sh"}
	if zsh, err := exec.LookPath("zsh"); err == nil {
		shells = append(shells, zsh)
	}
	store := filepath.Join(dir, "codex-threads", "current")
	for _, shell := range shells {
		for _, tc := range []struct {
			name  string
			env   []string
			start bool
		}{
			{"participant", []string{"AM_ME=codex", "AM_ROOT=" + root}, true},
			{"no AM_ME and no store", nil, false},
		} {
			for _, hook := range codexDialogHooks(fake, store) {
				if hook.Event != "PermissionRequest" {
					continue
				}
				_ = os.Remove(ran)
				cmd := exec.Command(shell, "-c", hook.Command)
				cmd.Dir = dir
				cmd.Env = append([]string{"PATH=/usr/bin:/bin", "RAN=" + ran}, tc.env...)
				cmd.Stdin = strings.NewReader(`{"hook_event_name":"PermissionRequest"}`)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s %s: %v: %s", shell, tc.name, err, out)
				}
				if _, err := os.Stat(ran); (err == nil) != tc.start {
					t.Errorf("%s %s: started amq = %v, want %v", shell, tc.name, err == nil, tc.start)
				}
			}
		}
	}

	clearSendMailboxTestEnv(t)
	t.Setenv(envMe, "codex")
	t.Setenv(envRoot, root)
	marker := filepath.Join(fsq.AgentBase(root, "codex"), wakeDialogFileName)
	hook := func(verb, input string) {
		t.Helper()
		if err := runWakeDialog([]string{"--harness", "codex", verb}, strings.NewReader(input)); err != nil {
			t.Fatalf("wake dialog %s: %v", verb, err)
		}
	}
	held := func() bool {
		_, err := os.Lstat(marker)
		return err == nil
	}
	hook("open", `{"session_id":"01a165af-5f6d-7270-bb60-671490b46ea1","turn_id":"t-1","hook_event_name":"PermissionRequest",`+
		`"tool_name":"Bash","tool_input":{"command":"touch hello.txt","description":"Allow running touch hello.txt outside the read-only sandbox?"}}`)
	if !held() {
		t.Fatal("PermissionRequest wrote no marker")
	}
	hook("close", `{"session_id":"01a165af-5f6d-7270-bb60-671490b46ea1","turn_id":"t-1","hook_event_name":"Stop"}`)
	if held() {
		t.Fatal("the turn's Stop kept the hold")
	}

	// post-release review v0.113.2 finding S3: the Stop of a daemon turn
	// admitted before this terminal started removed its Startup marker.
	// Only a prompt typed in the launch releases it; this test process
	// stands in for the launch.
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	selfJSON, err := json.Marshal(self)
	if err != nil {
		t.Fatal(err)
	}
	startup := `{"schema":1,"event":"Startup","harness":"codex","kind":"hook_review","cwd":"/work","opened_at":"x","launch":` + string(selfJSON) + `}` + "\n"
	if err := os.WriteFile(marker, []byte(startup), 0o600); err != nil {
		t.Fatal(err)
	}
	hook("close", `{"session_id":"01a165af-5f6d-7270-bb60-671490b46ea1","turn_id":"t-0","hook_event_name":"Stop"}`)
	if !held() {
		t.Fatal("an earlier turn's Stop released the Startup hold")
	}
	hook("close", `{"session_id":"01a165af-5f6d-7270-bb60-671490b46ea1","turn_id":"t-2","hook_event_name":"UserPromptSubmit"}`)
	if held() {
		t.Fatal("the typed prompt kept the Startup hold")
	}
}

// Regression: #1181 review (Codex P1), two rounds. Codex gives a prompt no
// call token. A parallel call A that Codex allowed without asking emits no
// PermissionRequest, and its PostToolUse can carry the same tool and input
// as call B whose prompt still shows; the guard closed on that PostToolUse.
// The installed entries now have no PostToolUse, the receiver ignores one
// from an older entry, and only a turn or session event closes the hold.
func TestCodexDialogHookKeepsTheHoldWhenAnAllowedParallelCallCompletes(t *testing.T) {
	for _, hook := range codexDialogHooks("/opt/amq", "/store") {
		if hook.Event == "PostToolUse" {
			t.Fatal("the Codex guard installs a PostToolUse close")
		}
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(fsq.AgentBase(root, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	clearSendMailboxTestEnv(t)
	t.Setenv(envMe, "codex")
	t.Setenv(envRoot, root)
	marker := filepath.Join(fsq.AgentBase(root, "codex"), wakeDialogFileName)
	hook := func(verb, input string) {
		t.Helper()
		if err := runWakeDialog([]string{"--harness", "codex", verb}, strings.NewReader(input)); err != nil {
			t.Fatalf("wake dialog %s: %v", verb, err)
		}
	}
	held := func() bool {
		_, err := os.Lstat(marker)
		return err == nil
	}
	// B asks; A, allowed, runs and completes with the same command.
	hook("open", `{"turn_id":"t-1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test","description":"Allow make test?"}}`)
	hook("close", `{"turn_id":"t-1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"make test"},"tool_use_id":"exec-a"}`)
	if !held() {
		t.Fatal("allowed call A's completion released the hold while B's prompt still showed")
	}
	hook("close", `{"turn_id":"t-1","hook_event_name":"Interrupt"}`)
	if held() {
		t.Fatal("Interrupt kept the hold")
	}
}

// Regression: #1181 review (Codex P2). Doctor said installed when a
// trusted_hash was merely present, and ignored a hook disabled in /hooks;
// Codex runs a hook only when the stored hash equals its hash of the current
// definition and the hook is enabled, and an async open lets the prompt
// show before the marker. The hash vectors are what Codex 0.163.0 stored
// after "Trust all" for these exact entries.
func TestCodexDialogDoctorRequiresTheCurrentTrustHashAndEnabled(t *testing.T) {
	vectors := map[string]string{
		"PermissionRequest": "sha256:c0774a72a65ea5d80ac2ae8fc216954bba006d065793e3eb4a929d08a59b50ac",
		"SessionStart":      "sha256:220511253a2c437a12911143d52ac52aa94c645253dec93d27b35d5569bb9678",
		"SessionEnd":        "sha256:d820d76c36aae0f6a22b69e43d37b702064454f7f94b5490c191ab5a549f2597",
		"UserPromptSubmit":  "sha256:5cd1b3ed1e72d4e42208001df94a752d50ec7a9128860b816dd908bdceea9e17",
		"Stop":              "sha256:ea1a9a8e05218b3c5cd9a4a84f7f552d9e440c740337dcb52966bb06a9a3bfa8",
		"Interrupt":         "sha256:2f4bc142493a7c72b8392f4537f5e8ee40b0d0df3f0e6eae3eb2fc726bca1c90",
	}
	vecPath := filepath.Join(t.TempDir(), "hooks.json")
	if err := claude.InstallCommandHooksAt(vecPath, codexDialogHooks("/opt/amq/bin/amq", "/home/user/.amq/codex-threads/current")); err != nil {
		t.Fatal(err)
	}
	vecEntries, err := readCodexHookEntries(vecPath)
	if err != nil {
		t.Fatal(err)
	}
	for event, want := range vectors {
		if got := vecEntries[event][0].hash(event); got != want {
			t.Errorf("%s trust hash = %s, Codex stored %s", event, got, want)
		}
	}
	// A person's Stop hook: Codex drops the matcher of Stop and keeps statusMessage.
	status := "s"
	if got := (codexHookEntry{matcherSet: true, Matcher: "ignored", Command: "true", Timeout: 7, StatusMessage: &status}).hash("Stop"); got != "sha256:2a979f53236556da2a090ad26b2953c14bb033031c343116b154b104810259be" {
		t.Errorf("foreign Stop hook trust hash = %s", got)
	}

	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	codexRequirementsPath = filepath.Join(home, "no-requirements.toml")
	t.Cleanup(func() { codexRequirementsPath = "/etc/codex/requirements.toml" })
	bin := filepath.Join(home, "amq")
	if err := os.WriteFile(bin, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := codexDialogStore()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(codexHome, "hooks.json")
	if err := claude.InstallCommandHooksAt(path, codexDialogHooks(bin, store)); err != nil {
		t.Fatal(err)
	}
	entries, err := readCodexHookEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	writeState := func(hash func(event string) string, disabled string) {
		t.Helper()
		var b strings.Builder
		for event, list := range entries {
			fmt.Fprintf(&b, "[hooks.state.%q]\ntrusted_hash = %q\n", list[0].key(path, event), hash(event))
			if event == disabled {
				b.WriteString("enabled = false\n")
			}
		}
		if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	current := func(event string) string { return entries[event][0].hash(event) }
	for _, tc := range []struct {
		name     string
		hash     func(string) string
		disabled string
		want     string
	}{
		{"stale hash", func(string) string { return "sha256:00" }, "", "not trusted"},
		{"disabled", current, "Stop", "disabled"},
		{"trusted and enabled", current, "", "installed"},
	} {
		writeState(tc.hash, tc.disabled)
		check, ok := checkCodexDialogHook()
		if !ok || !strings.Contains(check.Message, tc.want) || (tc.want == "installed") != (check.Status == "ok") {
			t.Errorf("%s: doctor = %+v, want %q", tc.name, check, tc.want)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	async := strings.Replace(string(raw), `codex open || true","timeout":5}`, `codex open || true","timeout":5,"async":true}`, 1)
	if async == string(raw) {
		t.Fatal("setup: no PermissionRequest entry to make async")
	}
	if err := os.WriteFile(path, []byte(async), 0o600); err != nil {
		t.Fatal(err)
	}
	// Trust the async definition as it is, so only the async check can refuse it.
	if entries, err = readCodexHookEntries(path); err != nil {
		t.Fatal(err)
	}
	writeState(current, "")
	if check, _ := checkCodexDialogHook(); check.Status == "ok" || !strings.Contains(check.Message, "PermissionRequest") {
		t.Errorf("async open: doctor = %+v, want a stale PermissionRequest", check)
	}
}

// Pre-merge review of fix/wake-holds-0113 (Codex, binding rule 2): any
// UserPromptSubmit cleared any Codex Startup marker, including one adopted
// through the thread identity record and one from another client of the
// thread. Only a prompt hook with its own identity that runs under the
// marker's launch process releases it.
func TestCodexStartupMarkerClosesOnlyForAPromptOfItsLaunch(t *testing.T) {
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	other := exec.Command("sleep", "5")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	elsewhere, err := launch.HolderOf(other.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	prompt := wakeDialogHookInput{Event: "UserPromptSubmit", SessionID: "01a165af-5f6d-7270-bb60-671490b46ea1"}
	for _, tc := range []struct {
		name        string
		launch      launch.HolderIdentity
		ownIdentity bool
		closed      bool
	}{
		{"own identity under the launch", self, true, true},
		{"identity adopted through the thread record", self, false, false},
		{"another process tree", elsewhere, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			agentDir, err := openWakeAgentDir(root, "codex")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = agentDir.Close() })
			holder := tc.launch
			raw, err := json.Marshal(wakeDialogMarker{Schema: 1, Event: wakeDialogStartupEvent, Harness: "codex", Kind: wakeStartupKindReview, Cwd: "/work", Launch: &holder})
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(fsq.AgentBase(root, "codex"), wakeDialogFileName)
			if err := os.WriteFile(marker, append(raw, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			in := prompt
			in.ownIdentity = tc.ownIdentity
			if err := agentDir.withFD(func(dirfd int) error { return clearTurnDialogMarker(dirfd, agentDir, in) }); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); os.IsNotExist(err) != tc.closed {
				t.Fatalf("marker closed = %v, want %v", os.IsNotExist(err), tc.closed)
			}
		})
	}
}

// W1 review (Pro finding 2): a PermissionRequest from an older daemon turn
// replaced this launch's Codex Startup marker, and that turn's Stop then
// removed it while the startup screen still showed.
func TestCodexTurnOpenKeepsAStartupMarker(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	clearSendMailboxTestEnv(t)
	t.Setenv(envMe, "codex")
	t.Setenv(envRoot, root)
	marker := filepath.Join(fsq.AgentBase(root, "codex"), wakeDialogFileName)
	startup := `{"schema":1,"event":"Startup","harness":"codex","kind":"hook_review","cwd":"/work","opened_at":"x"}` + "\n"
	if err := os.WriteFile(marker, []byte(startup), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ verb, input string }{
		{"open", `{"session_id":"01a165af-5f6d-7270-bb60-671490b46ea1","turn_id":"t-old","hook_event_name":"PermissionRequest"}`},
		{"close", `{"session_id":"01a165af-5f6d-7270-bb60-671490b46ea1","turn_id":"t-old","hook_event_name":"Stop"}`},
	} {
		if err := runWakeDialog([]string{"--harness", "codex", step.verb}, strings.NewReader(step.input)); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != startup {
		t.Fatalf("Startup marker after an older turn's open and Stop = %q, %v", raw, err)
	}
}
