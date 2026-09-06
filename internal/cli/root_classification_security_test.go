package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRootLocalAmqrcCanDeclareLegitimateCustomBase(t *testing.T) {
	t.Setenv("AM_BASE_ROOT", "")

	p := t.TempDir()
	baseRoot := filepath.Join(p, "queue")
	sessionRoot := filepath.Join(baseRoot, "collab")
	if err := os.MkdirAll(filepath.Join(sessionRoot, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, ".amqrc"), []byte(`{"root":"."}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := configuredBaseRoot(baseRoot); got != baseRoot {
		t.Errorf("configuredBaseRoot(%q) = %q, want self-declared base %q", baseRoot, got, baseRoot)
	}
	if got := classifyRoot(baseRoot); got != "" {
		t.Errorf("classifyRoot(%q) = %q, want empty for configured base", baseRoot, got)
	}
	if got := classifyRoot(sessionRoot); got != baseRoot {
		t.Errorf("classifyRoot(%q) = %q, want configured base %q", sessionRoot, got, baseRoot)
	}
	if !sameBaseTree(baseRoot, sessionRoot) {
		t.Errorf("sameBaseTree(%q, %q) = false, want true", baseRoot, sessionRoot)
	}
}

func TestRoutedSessionRefusesRetargetedBaseIdentity(t *testing.T) {
	baseOne := t.TempDir()
	baseTwo := t.TempDir()
	for _, base := range []string{baseOne, baseTwo} {
		for _, session := range []string{"session1", "session2"} {
			if err := fsq.EnsureAgentDirs(filepath.Join(base, session), "alice"); err != nil {
				t.Fatal(err)
			}
		}
	}
	alias := filepath.Join(t.TempDir(), "base")
	if err := os.Symlink(baseOne, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	baseToken, err := resolveTreeIdentityToken(alias)
	if err != nil {
		t.Fatal(err)
	}
	rootToken, err := resolveTreeIdentityToken(filepath.Join(alias, "session1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(baseTwo, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRoot, filepath.Join(alias, "session1"))
	t.Setenv(envBaseRoot, alias)
	t.Setenv(envSession, "session1")
	t.Setenv(envRootID, rootToken)
	t.Setenv(envBaseRootID, baseToken)

	err = runDrain([]string{"--me", "alice", "--session", "session2"})
	if err == nil || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("routed session accepted retargeted base identity: %v", err)
	}
}

func TestEnvEmitsAuthoritativeIdentityTokens(t *testing.T) {
	root := t.TempDir()
	result := runEnvJSONForTest(t, "--root", root, "--me", "alice")
	if result.RootID == "" || result.BaseRootID == "" {
		t.Fatalf("env omitted authoritative identities: %+v", result)
	}
	if verifyTreeIdentityToken(root, result.RootID) != TreeRelationSame ||
		verifyTreeIdentityToken(root, result.BaseRootID) != TreeRelationSame {
		t.Fatalf("env emitted unverifiable identities: %+v", result)
	}
}

func TestSendRefusesCrossTreeEscapeFromMisclassifiedRoot(t *testing.T) {
	t.Run("nested default-name session", func(t *testing.T) {
		p := t.TempDir()
		sourceRoot := filepath.Join(p, ".agent-mail", ".agent-mail")
		foreignRoot := filepath.Join(p, "foreign")
		ensureBypassMailboxes(t, sourceRoot, foreignRoot)
		targetRoot := filepath.Join(sourceRoot, "escape")
		if err := os.Symlink(foreignRoot, targetRoot); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}

		assertCrossTreeEscapeRefused(t, sourceRoot, targetRoot, foreignRoot)
	})

	t.Run("root-local amqrc", func(t *testing.T) {
		p := t.TempDir()
		sourceRoot := filepath.Join(p, ".agent-mail", "collab")
		foreignRoot := filepath.Join(p, "foreign")
		ensureBypassMailboxes(t, sourceRoot, foreignRoot)
		if err := os.WriteFile(filepath.Join(sourceRoot, ".amqrc"), []byte(`{"root":"."}`), 0o600); err != nil {
			t.Fatal(err)
		}
		targetRoot := filepath.Join(sourceRoot, "escape")
		if err := os.Symlink(foreignRoot, targetRoot); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}

		assertCrossTreeEscapeRefused(t, sourceRoot, targetRoot, foreignRoot)
	})
}

func ensureBypassMailboxes(t *testing.T, sourceRoot, foreignRoot string) {
	t.Helper()
	if err := fsq.EnsureAgentDirs(sourceRoot, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(foreignRoot, "bob"); err != nil {
		t.Fatal(err)
	}
}

func assertCrossTreeEscapeRefused(t *testing.T, sourceRoot, targetRoot, foreignRoot string) {
	t.Helper()
	t.Setenv("AM_ROOT", sourceRoot)
	t.Setenv("AM_BASE_ROOT", "")
	setOptionalEnv(t, "AM_SESSION", "", false)

	err := runSend([]string{"--root", targetRoot, "--me", "alice", "--to", "bob", "--body", "must not escape"})
	if err == nil || !strings.Contains(err.Error(), "refusing send") {
		t.Fatalf("expected cross-tree refusal, got %v", err)
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if got := inboxCount(t, foreignRoot, "bob"); got != 0 {
		t.Fatalf("foreign inbox received %d messages, want 0", got)
	}
}
