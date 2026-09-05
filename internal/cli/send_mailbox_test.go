package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSendCompletesConfiguredDestinationMailboxBeforeDelivery(t *testing.T) {
	root := initializedSendMailboxRoot(t, "claude", "codex")
	if err := os.RemoveAll(filepath.Join(root, "agents", "claude")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "claude",
			"--subject", "configured",
			"--body", "hello",
		})
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	assertCompleteSendMailbox(t, root, "claude")

	listed, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--root", root, "--me", "claude", "--new"})
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(listed, "configured") {
		t.Fatalf("list output missing delivered subject: %q", listed)
	}

	drained, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "claude", "--include-body"})
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !strings.Contains(drained, "hello") {
		t.Fatalf("drain output missing delivered body: %q", drained)
	}
}

func TestSendCompletesUnknownDestinationMailboxInInitializedRoot(t *testing.T) {
	root := initializedSendMailboxRoot(t, "claude", "codex")

	_, stderr, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "clade",
			"--subject", "unknown",
			"--body", "hello",
		})
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(stderr, `warning: handle "clade" not in config.json`) {
		t.Fatalf("unknown-handle warning changed: %q", stderr)
	}
	if !strings.Contains(stderr, "amq who --json") || !strings.Contains(stderr, "message may not be read") {
		t.Fatalf("unknown-handle warning missing verification remedy: %q", stderr)
	}
	assertCompleteSendMailbox(t, root, "clade")

	listed, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--root", root, "--me", "clade", "--new"})
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(listed, "unknown") {
		t.Fatalf("list output missing delivered subject: %q", listed)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "clade", "--include-body"})
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestSendStrictUnknownDestinationDoesNotCreateMailbox(t *testing.T) {
	root := initializedSendMailboxRoot(t, "claude", "codex")

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "clade",
			"--strict",
			"--body", "hello",
		})
	})
	if err == nil || !strings.Contains(err.Error(), `handle "clade" not in config.json`) {
		t.Fatalf("strict send error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "agents", "clade")); !os.IsNotExist(statErr) {
		t.Fatalf("strict unknown send created a mailbox: %v", statErr)
	}
}

func TestSendStrictPresentEmptyConfigStillValidatesEffectiveRoster(t *testing.T) {
	root := initializedSendMailboxRoot(t)

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", reservedHumanHandle,
			"--to", "typo",
			"--strict",
			"--body", "hello",
		})
	})
	if err == nil || !strings.Contains(err.Error(), `handle "typo" not in config.json`) {
		t.Fatalf("strict empty-config send error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "agents", "typo")); !os.IsNotExist(statErr) {
		t.Fatalf("strict empty-config send created typo mailbox: %v", statErr)
	}
}

func TestSendRefusesUninitializedRootWithoutWriting(t *testing.T) {
	root := t.TempDir()
	clearSendMailboxTestEnv(t)

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "claude",
			"--body", "hello",
		})
	})
	if GetExitCode(err) != ExitNotFound {
		t.Fatalf("send error = %v (exit %d), want not-found exit %d", err, GetExitCode(err), ExitNotFound)
	}
	for _, remedy := range []string{"amq init", "amq coop init"} {
		if !strings.Contains(err.Error(), remedy) {
			t.Fatalf("uninitialized-root error missing %q: %v", remedy, err)
		}
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("uninitialized-root send wrote entries: %#v", entries)
	}
}

func TestSendConfiguredMailboxRepairRefusesSymlinkWithoutDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform privileges")
	}
	root := initializedSendMailboxRoot(t, "claude", "codex")
	cur := fsq.AgentInboxCur(root, "claude")
	if err := os.Remove(cur); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, cur); err != nil {
		t.Fatal(err)
	}

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "claude",
			"--body", "must not deliver",
		})
	})
	if err == nil {
		t.Fatal("send through incomplete symlinked mailbox succeeded")
	}
	entries, readErr := os.ReadDir(fsq.AgentInboxNew(root, "claude"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("refused repair delivered messages: %#v", entries)
	}
	info, statErr := os.Lstat(cur)
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("repair changed symlink: info=%v err=%v", info, statErr)
	}
}

func TestSendUnknownMailboxRepairRefusesSymlinkWithoutDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform privileges")
	}
	root := initializedSendMailboxRoot(t, "claude", "codex")
	outside := t.TempDir()
	unknownInbox := filepath.Join(root, "agents", "clade", "inbox")
	if err := os.MkdirAll(filepath.Dir(unknownInbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, unknownInbox); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "clade",
			"--body", "must not deliver",
		})
	})
	if err == nil {
		t.Fatal("send through unknown symlinked mailbox succeeded")
	}
	if !strings.Contains(stderr, `warning: handle "clade" not in config.json`) {
		t.Fatalf("unknown-handle warning changed: %q", stderr)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("refused repair wrote through symlink: %#v", entries)
	}
	info, statErr := os.Lstat(unknownInbox)
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("repair changed symlink: info=%v err=%v", info, statErr)
	}
}

func TestSendRepairsBaseOnlyCoopMailboxAcrossSessionRoutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(base string) []string
		pin  bool
	}{
		{
			name: "same session",
			args: func(string) []string {
				return []string{"--me", "alice", "--to", "bob", "--body", "same"}
			},
			pin: true,
		},
		{
			name: "target session",
			args: func(string) []string {
				return []string{"--me", "alice", "--to", "bob", "--session", "qa", "--body", "target"}
			},
			pin: true,
		},
		{
			name: "from session",
			args: func(base string) []string {
				return []string{"--root", base, "--from-session", "collab", "--me", "alice", "--to", "bob", "--session", "qa", "--body", "from"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSendMailboxTestEnv(t)
			base := filepath.Join(t.TempDir(), ".agent-mail")
			source := filepath.Join(base, "collab")
			target := source
			if tc.name != "same session" {
				target = filepath.Join(base, "qa")
			}
			configureSendTestRoot(t, base, "alice", "bob")
			configPath := filepath.Join(base, "meta", "config.json")
			configBefore, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, root := range dedupeStrings([]string{source, target}) {
				if err := fsq.EnsureRootDirs(root); err != nil {
					t.Fatal(err)
				}
			}
			if err := fsq.EnsureAgentDirs(source, "alice"); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(target, "bob"); err != nil {
				t.Fatal(err)
			}
			missing := fsq.AgentDLQCur(target, "bob")
			if err := os.Remove(missing); err != nil {
				t.Fatal(err)
			}
			if tc.pin {
				pinSendSessionForTest(t, base, source, "collab")
			}

			if _, _, err := captureEnvOutput(t, func() error {
				return runSend(tc.args(base))
			}); err != nil {
				t.Fatalf("send: %v", err)
			}
			assertCompleteSendMailbox(t, target, "bob")
			if _, err := os.Lstat(filepath.Join(target, "meta", "config.json")); !os.IsNotExist(err) {
				t.Fatalf("send copied config into session: %v", err)
			}
			configAfter, err := os.ReadFile(configPath)
			if err != nil || string(configAfter) != string(configBefore) {
				t.Fatalf("send changed base config: err=%v before=%q after=%q", err, configBefore, configAfter)
			}
			entries, err := os.ReadDir(fsq.AgentInboxNew(target, "bob"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("delivered entries = %d, err=%v", len(entries), err)
			}
		})
	}
}

func TestSendInvalidMessageDoesNotRepairMailbox(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	missing := fsq.AgentDLQCur(root, "bob")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "alice",
			"--to", "bob",
			"--priority", "not-a-priority",
			"--body", "invalid",
		})
	})
	if err == nil {
		t.Fatal("invalid send succeeded")
	}
	if _, statErr := os.Lstat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("invalid send repaired mailbox: %v", statErr)
	}
}

func TestSendBaseOnlyCoopConfigPreservesUnknownHandleModes(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	session := filepath.Join(base, "collab")
	configureSendTestRoot(t, base, "alice", "bob")
	if err := fsq.EnsureAgentDirs(session, "alice"); err != nil {
		t.Fatal(err)
	}
	pinSendSessionForTest(t, base, session, "collab")

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{"--me", "alice", "--to", "typo", "--strict", "--body", "strict"})
	})
	if err == nil || !strings.Contains(err.Error(), `handle "typo" not in config.json`) {
		t.Fatalf("strict base-config error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(session, "agents", "typo")); !os.IsNotExist(statErr) {
		t.Fatalf("strict base-config send created mailbox: %v", statErr)
	}

	_, stderr, err := captureEnvOutput(t, func() error {
		return runSend([]string{"--me", "alice", "--to", "typo", "--body", "warn"})
	})
	if err != nil {
		t.Fatalf("non-strict base-config send: %v", err)
	}
	if !strings.Contains(stderr, `warning: handle "typo" not in config.json`) {
		t.Fatalf("non-strict warning changed: %q", stderr)
	}
	assertCompleteSendMailbox(t, session, "typo")
}

func TestSendExplicitUnpinnedSessionUsesBaseOnlyCoopConfig(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	session := filepath.Join(base, "dev")
	configureSendTestRoot(t, base, "alice", "bob")
	configPath := filepath.Join(base, "meta", "config.json")
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(session, "alice"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", session,
			"--me", "alice",
			"--to", "bob",
			"--strict",
			"--body", "explicit unpinned session",
			"--json",
		})
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	assertCompleteSendMailbox(t, session, "bob")
	if got := inboxCount(t, session, "bob"); got != 1 {
		t.Fatalf("delivered messages = %d, want 1", got)
	}
	if _, err := os.Lstat(filepath.Join(session, "meta", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("send copied base config into session: %v", err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || string(configAfter) != string(configBefore) {
		t.Fatalf("send changed base config: err=%v before=%q after=%q", err, configBefore, configAfter)
	}
}

func TestLocalMailboxConfigAuthorityPrecedence(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	session := filepath.Join(base, "dev")
	foreignBase := filepath.Join(t.TempDir(), "custom-base")

	for _, test := range []struct {
		name       string
		root       string
		pin        sessionPin
		ignorePin  bool
		wantBase   string
		wantBaseID string
	}{
		{
			name:     "classified unpinned session",
			root:     session,
			wantBase: base,
		},
		{
			name: "named identity pin",
			root: session,
			pin: sessionPin{
				Present:     true,
				Session:     "dev",
				BaseRoot:    foreignBase,
				IdentityPin: true,
				BaseRootID:  "base-id",
			},
			wantBase:   foreignBase,
			wantBaseID: "base-id",
		},
		{
			name: "sessionless pin stays exact",
			root: session,
			pin: sessionPin{
				Present:     true,
				BaseRoot:    session,
				IdentityPin: true,
				BaseRootID:  "exact-root-id",
			},
		},
		{
			name: "ignored named pin uses classified root",
			root: session,
			pin: sessionPin{
				Present:     true,
				Session:     "foreign",
				BaseRoot:    foreignBase,
				IdentityPin: true,
				BaseRootID:  "foreign-base-id",
			},
			ignorePin: true,
			wantBase:  base,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotBase, gotBaseID := localMailboxConfigAuthority(test.root, test.pin, test.ignorePin)
			if gotBase != test.wantBase || gotBaseID != test.wantBaseID {
				t.Fatalf(
					"authority = (%q, %q), want (%q, %q)",
					gotBase,
					gotBaseID,
					test.wantBase,
					test.wantBaseID,
				)
			}
		})
	}
}

func TestSendRepairsOnlyRequestedRecipients(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob", "carol")
	bobMissing := fsq.AgentDLQCur(root, "bob")
	carolMissing := fsq.AgentDLQCur(root, "carol")
	for _, path := range []string{bobMissing, carolMissing} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{"--root", root, "--me", "alice", "--to", "bob", "--body", "targeted"})
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if info, err := os.Stat(bobMissing); err != nil || !info.IsDir() {
		t.Fatalf("requested mailbox was not repaired: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(carolMissing); !os.IsNotExist(err) {
		t.Fatalf("unrequested mailbox was repaired: %v", err)
	}
}

func TestSendMixedRecipientRepairPreflightsAsOneTransaction(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	bobMissing := fsq.AgentDLQCur(root, "bob")
	if err := os.Remove(bobMissing); err != nil {
		t.Fatal(err)
	}
	carolInbox := filepath.Join(root, "agents", "carol", "inbox")
	if err := os.MkdirAll(filepath.Dir(carolInbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(carolInbox, []byte("unsafe"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "alice",
			"--to", "bob,carol,bob",
			"--thread", "transactional",
			"--body", "must not partially repair",
		})
	})
	if err == nil {
		t.Fatal("mixed unsafe send succeeded")
	}
	if _, statErr := os.Lstat(bobMissing); !os.IsNotExist(statErr) {
		t.Fatalf("failed transaction repaired safe recipient first: %v", statErr)
	}
}

func pinSendSessionForTest(t *testing.T, base, root, session string) {
	t.Helper()
	baseID, err := resolveTreeIdentityToken(base)
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := resolveTreeIdentityToken(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envBaseRoot, base)
	t.Setenv(envRoot, root)
	t.Setenv(envSession, session)
	t.Setenv(envBaseRootID, baseID)
	t.Setenv(envRootID, rootID)
}

func initializedSendMailboxRoot(t *testing.T, agents ...string) string {
	t.Helper()
	clearSendMailboxTestEnv(t)
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	configureSendTestRoot(t, root, agents...)
	return root
}

func configureSendTestRoot(t *testing.T, root string, agents ...string) {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "meta", "config.json")
	cfg := config.Config{Version: 1}
	if existing, err := config.LoadConfig(configPath); err == nil {
		cfg = existing
	} else if !os.IsNotExist(err) {
		t.Fatalf("LoadConfig(%s): %v", configPath, err)
	}
	seen := make(map[string]bool, len(cfg.Agents)+len(agents))
	for _, agent := range cfg.Agents {
		seen[agent] = true
	}
	for _, agent := range agents {
		if !seen[agent] {
			seen[agent] = true
			cfg.Agents = append(cfg.Agents, agent)
		}
	}
	if err := config.WriteConfig(configPath, cfg, true); err != nil {
		t.Fatal(err)
	}
}

func assertCompleteSendMailbox(t *testing.T, root, agent string) {
	t.Helper()
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		path := fsq.AgentMailboxPath(root, agent, leaf)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("required mailbox path is not a directory: %s info=%v err=%v", path, info, err)
		}
	}
}

func clearSendMailboxTestEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		envRoot,
		envMe,
		envBaseRoot,
		envSession,
		envGlobalRoot,
		envRootID,
		envBaseRootID,
	} {
		setOptionalEnv(t, key, "", false)
	}
}
