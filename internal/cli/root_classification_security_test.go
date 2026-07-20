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

func TestCaseInsensitiveDefaultRootSpellingStillRefusesBothEscapes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sessionName string
		writeRoot   bool
	}{
		{name: "nested default-name session", sessionName: ".agent-mail"},
		{name: "root-local amqrc", sessionName: "collab", writeRoot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := t.TempDir()
			if !caseInsensitiveFS(t, p) {
				t.Skip("case-sensitive filesystem")
			}
			foreign := t.TempDir()
			base := filepath.Join(p, ".agent-mail")
			session := filepath.Join(base, tc.sessionName)
			if err := fsq.EnsureAgentDirs(session, "alice"); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(foreign, "bob"); err != nil {
				t.Fatal(err)
			}
			if tc.writeRoot {
				if err := os.WriteFile(filepath.Join(session, ".amqrc"), []byte(`{"root":"."}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sessionName == ".agent-mail" {
				if err := os.MkdirAll(filepath.Join(p, "poison", "agents"), 0o700); err != nil {
					t.Fatal(err)
				}
				if got := classifyRoot(filepath.Join(p, ".AGENT-MAIL")); got != "" {
					t.Fatalf("case-insensitive default base with poison sibling classified as %q, want base", got)
				}
			}
			upperBase := filepath.Join(p, ".AGENT-MAIL")
			source := filepath.Join(upperBase, tc.sessionName)
			target := filepath.Join(source, "escape")
			if err := os.Symlink(foreign, target); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
			assertCrossTreeEscapeRefused(t, source, target, foreign)
		})
	}
}

func TestIsDefaultCoopRootDoesNotFoldCaseOnCaseSensitiveFS(t *testing.T) {
	p := t.TempDir()
	lower := filepath.Join(p, defaultCoopRoot)
	upper := filepath.Join(p, ".AGENT-MAIL")
	if err := os.MkdirAll(lower, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	lowerInfo, err := os.Lstat(lower)
	if err != nil {
		t.Fatal(err)
	}
	upperInfo, err := os.Lstat(upper)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(lowerInfo, upperInfo) {
		t.Skip("case-insensitive filesystem")
	}
	if isDefaultCoopRoot(upper) {
		t.Fatal("isDefaultCoopRoot folded distinct case-sensitive directory names")
	}
}

func caseInsensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	lower := filepath.Join(dir, ".probe-case")
	if err := os.MkdirAll(lower, 0o700); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(lower)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(filepath.Join(dir, ".PROBE-CASE"))
	return err == nil && os.SameFile(want, got)
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

func TestSameBaseTreeAllowsNonexistentSameTreeTarget(t *testing.T) {
	p := t.TempDir()
	base := filepath.Join(p, ".agent-mail")
	source := filepath.Join(base, "collab")
	target := filepath.Join(base, "newsession")
	if err := os.MkdirAll(filepath.Join(source, "agents", "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !sameBaseTree(source, target) {
		t.Fatal("same-tree target that delivery will create was treated as cross-tree")
	}
}

func TestSameBaseTreeAllowsSymlinkedCustomBase(t *testing.T) {
	realData := t.TempDir()
	repoParent := t.TempDir()
	repoQueue := filepath.Join(repoParent, "queue")
	if err := os.Symlink(realData, repoQueue); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	source := filepath.Join(repoQueue, "collab")
	target := filepath.Join(repoQueue, "auth")
	for _, root := range []string{source, target} {
		if err := os.MkdirAll(filepath.Join(root, "agents", "alice"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AM_BASE_ROOT", repoQueue)
	t.Setenv("AM_SESSION", "")
	if !sameBaseTree(source, target) {
		t.Fatal("custom base reached through a symlinked ancestor was treated as cross-tree")
	}
}

func TestEnvDoesNotLaunderSiblingHintIntoBaseRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "queue")
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "other", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRoot, "")
	t.Setenv(envBaseRoot, "")
	setOptionalEnv(t, envSession, "", false)

	if got := classifyRoot(root); got != "" {
		t.Fatalf("authority classification laundered sibling hint into base %q", got)
	}
	if got := classifyRootForDisplay(root); got != parent {
		t.Fatalf("display classification = %q, want sibling hint %q", got, parent)
	}
	result := runEnvJSONForTest(t, "--root", root, "--me", "alice")
	expectSamePath(t, result.BaseRoot, root)
	if result.InSession || result.SessionName != "" {
		t.Fatalf("heuristic root became session authority: %+v", result)
	}
}

func TestConflictingSourceRootStaleAMRootValidBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRoot, filepath.Join(t.TempDir(), "stale", "session"))
	t.Setenv(envBaseRoot, base)
	setOptionalEnv(t, envSession, "", false)

	if source, refused := conflictingSourceRoot(target); refused {
		t.Fatalf("stale AM_ROOT overrode valid same-tree AM_BASE_ROOT evidence: %q", source)
	}
}

func TestPinnedRootCanonicalAlias(t *testing.T) {
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	token, err := resolveTreeIdentityToken(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRoot, alias)
	t.Setenv(envBaseRoot, alias)
	t.Setenv(envSession, "")
	t.Setenv(envRootID, token)
	t.Setenv(envBaseRootID, token)

	mismatch, err := sessionPinMismatch(realRoot)
	if err != nil {
		t.Fatalf("canonical alias verification failed: %v", err)
	}
	if mismatch != nil {
		t.Fatalf("canonical alias rejected: %v", mismatch)
	}
}

func TestPinnedRootRetargetedAlias(t *testing.T) {
	realRoot := t.TempDir()
	replacement := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	token, err := resolveTreeIdentityToken(alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRoot, alias)
	t.Setenv(envBaseRoot, alias)
	t.Setenv(envSession, "")
	t.Setenv(envRootID, token)
	t.Setenv(envBaseRootID, token)

	mismatch, err := sessionPinMismatch(alias)
	if err != nil {
		t.Fatalf("retargeted alias check: %v", err)
	}
	if mismatch == nil {
		t.Fatal("retargeted alias was accepted by an identity-bound pin")
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

func TestPinnedRootIdentityTokenStates(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	rootToken, err := resolveTreeIdentityToken(root)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := resolveTreeIdentityToken(other)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("absent retains legacy semantics", func(t *testing.T) {
		t.Setenv(envRoot, root)
		t.Setenv(envBaseRoot, root)
		t.Setenv(envSession, "")
		setOptionalEnv(t, envRootID, "", false)
		setOptionalEnv(t, envBaseRootID, "", false)
		mismatch, err := sessionPinMismatch(root)
		if err != nil || mismatch != nil {
			t.Fatalf("legacy pin rejected: mismatch=%v err=%v", mismatch, err)
		}
	})

	t.Run("mismatch refuses", func(t *testing.T) {
		t.Setenv(envRoot, root)
		t.Setenv(envBaseRoot, root)
		t.Setenv(envSession, "")
		t.Setenv(envRootID, otherToken)
		t.Setenv(envBaseRootID, otherToken)
		mismatch, err := sessionPinMismatch(root)
		if err != nil {
			t.Fatal(err)
		}
		if mismatch == nil {
			t.Fatal("mismatched tokens were accepted")
		}
	})

	t.Run("unknown version refuses", func(t *testing.T) {
		t.Setenv(envRoot, root)
		t.Setenv(envBaseRoot, root)
		t.Setenv(envSession, "")
		t.Setenv(envRootID, strings.Replace(rootToken, "v1:", "v999:", 1))
		t.Setenv(envBaseRootID, strings.Replace(rootToken, "v1:", "v999:", 1))
		mismatch, err := sessionPinMismatch(root)
		if mismatch == nil && err == nil {
			t.Fatal("unknown token version was accepted")
		}
	})
}

func TestListWarnsOnUnverifiableIdentityToken(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRoot, root)
	t.Setenv(envBaseRoot, root)
	t.Setenv(envSession, "")
	t.Setenv(envRootID, "v999:test:opaque")
	t.Setenv(envBaseRootID, "v999:test:opaque")

	_, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("advisory list rejected unverifiable token: %v", err)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "unverifiable AMQ identity pin") {
		t.Fatalf("list warning = %q", stderr)
	}
}

func TestDoctorWarnsOnUnverifiableIdentityToken(t *testing.T) {
	root := t.TempDir()
	t.Setenv(envRoot, root)
	t.Setenv(envBaseRoot, root)
	t.Setenv(envSession, "")
	t.Setenv(envRootID, "v999:test:opaque")
	t.Setenv(envBaseRootID, "v999:test:opaque")

	check := checkSessionPinIdentity(root)
	if check.Status != "warn" || !strings.Contains(check.Message, "unverifiable AMQ identity pin") {
		t.Fatalf("doctor identity check = %+v, want warning", check)
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
