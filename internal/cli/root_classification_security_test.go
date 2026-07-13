package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestBypassNestedDefaultName(t *testing.T) {
	p := t.TempDir()
	foreign := t.TempDir()

	baseRoot := filepath.Join(p, ".agent-mail")
	sessionRoot := filepath.Join(baseRoot, ".agent-mail")
	if err := os.MkdirAll(filepath.Join(sessionRoot, "agents", "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(foreign, "agents", "bob"), 0o700); err != nil {
		t.Fatal(err)
	}
	targetRoot := filepath.Join(sessionRoot, "escape")
	if err := os.Symlink(foreign, targetRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_SESSION", "")
	t.Setenv("AM_ROOT", sessionRoot)

	if got := classifyRoot(sessionRoot); got != baseRoot {
		t.Errorf("classifyRoot(%q) = %q, want parent base %q", sessionRoot, got, baseRoot)
	}
	if src, refused := conflictingSourceRoot(targetRoot); !refused {
		t.Fatalf("cross-tree send guard did not refuse AM_ROOT=%q targeting %q (symlink to %q); source=%q", sessionRoot, targetRoot, foreign, src)
	}
}

func TestBypassRootLocalAmqrcDoesNotRebaseDefaultSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configRoot func(string) string
	}{
		{name: "relative dot", configRoot: func(string) string { return "." }},
		{name: "absolute self path", configRoot: func(root string) string { return root }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := t.TempDir()
			foreign := t.TempDir()

			baseRoot := filepath.Join(p, ".agent-mail")
			sessionRoot := filepath.Join(baseRoot, "collab")
			if err := os.MkdirAll(filepath.Join(sessionRoot, "agents", "alice"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(foreign, "agents", "bob"), 0o700); err != nil {
				t.Fatal(err)
			}
			config, err := json.Marshal(amqrc{Root: tc.configRoot(sessionRoot)})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sessionRoot, ".amqrc"), config, 0o600); err != nil {
				t.Fatal(err)
			}
			targetRoot := filepath.Join(sessionRoot, "escape")
			if err := os.Symlink(foreign, targetRoot); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}

			t.Setenv("AM_BASE_ROOT", "")
			t.Setenv("AM_SESSION", "")
			t.Setenv("AM_ROOT", sessionRoot)

			if got := classifyRoot(sessionRoot); got != baseRoot {
				t.Errorf("classifyRoot(%q) = %q, want parent base %q", sessionRoot, got, baseRoot)
			}
			if src, refused := conflictingSourceRoot(targetRoot); !refused {
				t.Fatalf("cross-tree send guard did not refuse AM_ROOT=%q targeting %q (symlink to %q); source=%q", sessionRoot, targetRoot, foreign, src)
			}
		})
	}
}

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

func TestClassifyRootConfiguredCustomBaseSurvivesPoisonSibling(t *testing.T) {
	t.Setenv("AM_BASE_ROOT", "")

	p := t.TempDir()
	baseRoot := filepath.Join(p, "queue")
	sessionRoot := filepath.Join(baseRoot, "collab")
	if err := os.WriteFile(filepath.Join(p, ".amqrc"), []byte(`{"root":"queue"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sessionRoot, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p, ".claude", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}

	if got := classifyRoot(baseRoot); got != "" {
		t.Errorf("classifyRoot(%q) = %q, want empty for configured base", baseRoot, got)
	}
	if got := classifyRoot(sessionRoot); got != baseRoot {
		t.Errorf("classifyRoot(%q) = %q, want configured base %q", sessionRoot, got, baseRoot)
	}
	if got := baseRootOf(baseRoot); got != baseRoot {
		t.Errorf("baseRootOf(%q) = %q, want %q", baseRoot, got, baseRoot)
	}
	if !sameBaseTree(baseRoot, sessionRoot) {
		t.Errorf("sameBaseTree(%q, %q) = false, want true", baseRoot, sessionRoot)
	}
}

func TestIsBaseOrSessionRootRejectsEmptyInput(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		root string
		base string
	}{
		{name: "empty root", root: "", base: cwd},
		{name: "blank root", root: "   ", base: cwd},
		{name: "empty base", root: cwd, base: ""},
		{name: "blank base", root: cwd, base: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if isBaseOrSessionRoot(tc.root, tc.base) {
				t.Fatalf("isBaseOrSessionRoot(%q, %q) = true, want false", tc.root, tc.base)
			}
		})
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
