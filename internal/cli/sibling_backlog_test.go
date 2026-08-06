package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDrainEmptyHintsSiblingBacklogWithoutCorruptingJSON(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	siblingRoot := sessionRoot(t, parent, "session1", "alice")
	deliverGuardMessage(t, siblingRoot, "alice", "waiting-1")
	deliverGuardMessage(t, siblingRoot, "alice", "waiting-2")
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(siblingRoot, "alice"), ".ignored.md"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write dotfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(siblingRoot, "alice"), "ignored.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write non-message: %v", err)
	}

	t.Setenv("AM_ROOT", currentRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "collab")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if !strings.Contains(stdout, `"count": 0`) {
		t.Fatalf("stdout must remain valid empty drain JSON, got %q", stdout)
	}
	assertSiblingHint(t, stderr, 2, "alice", "session1")
}

func TestListEmptyJSONHintsSiblingBacklogWithoutCorruptingJSON(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	siblingRoot := sessionRoot(t, parent, "session1", "alice")
	deliverGuardMessage(t, siblingRoot, "alice", "waiting")

	t.Setenv("AM_ROOT", currentRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "collab")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("runList: %v", err)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("stdout must remain valid empty-list JSON, got %q", stdout)
	}
	assertSiblingHint(t, stderr, 1, "alice", "session1")
}

func TestDrainEmptyHintsBaseBacklogWithoutCorruptingJSON(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "project with space")
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	if err := fsq.EnsureAgentDirs(baseRoot, "alice"); err != nil {
		t.Fatalf("ensure base mailbox: %v", err)
	}
	deliverGuardMessage(t, baseRoot, "alice", "waiting-at-base")

	t.Setenv("AM_ROOT", currentRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "collab")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if !strings.Contains(stdout, `"count": 0`) {
		t.Fatalf("stdout must remain valid empty drain JSON, got %q", stdout)
	}
	assertBaseHint(t, stderr, 1, "alice", baseRoot, "collab")
}

func TestListEmptyJSONHintsBaseBacklogWithoutCorruptingJSON(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "project with space")
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	if err := fsq.EnsureAgentDirs(baseRoot, "alice"); err != nil {
		t.Fatalf("ensure base mailbox: %v", err)
	}
	deliverGuardMessage(t, baseRoot, "alice", "waiting-at-base")

	t.Setenv("AM_ROOT", currentRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "collab")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("runList: %v", err)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("stdout must remain valid empty-list JSON, got %q", stdout)
	}
	assertBaseHint(t, stderr, 1, "alice", baseRoot, "collab")
}

func TestListEmptySessionWithoutBaseBacklogStaysQuiet(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")

	t.Setenv("AM_ROOT", currentRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "collab")

	_, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("runList: %v", err)
	}
	if strings.Contains(stderr, "base root") {
		t.Fatalf("empty base root must stay quiet: %q", stderr)
	}
}

func TestListEmptyBaseRootHintsSiblingBacklog(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	siblingRoot := sessionRoot(t, parent, "session1", "alice")
	deliverGuardMessage(t, siblingRoot, "alice", "waiting")

	t.Setenv("AM_ROOT", baseRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new"})
	})
	if err == nil || GetExitCode(err) != ExitNotFound {
		t.Fatalf("missing base-root mailbox should be not-found, got %v", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no false empty-list output", stdout)
	}
	assertSiblingHint(t, stderr, 1, "alice", "session1")
}

func TestListFilteredEmptyDoesNotHintWhenCurrentInboxHasMessages(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	siblingRoot := sessionRoot(t, parent, "session1", "alice")
	deliverGuardMessage(t, currentRoot, "alice", "current")
	deliverGuardMessage(t, siblingRoot, "alice", "sibling")

	t.Setenv("AM_ROOT", currentRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "collab")

	_, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--from", "carol"})
	})
	if err != nil {
		t.Fatalf("runList: %v", err)
	}
	if strings.Contains(stderr, "pending for") {
		t.Fatalf("filtered-empty results must not claim the current inbox is empty: %q", stderr)
	}
}

func TestListWarnsOnPinnedSessionMismatch(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "wrong-context")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("list remains an inspection path, got %v", err)
	}
	if !strings.Contains(stdout, `"id": "wrong-context"`) {
		t.Fatalf("list did not inspect resolved target: %q", stdout)
	}
	if !strings.Contains(stderr, "warning: session context mismatch") ||
		!strings.Contains(stderr, targetRoot) ||
		!strings.Contains(stderr, filepath.Join(baseRoot, "session1")) {
		t.Fatalf("list warning must name actual and pinned roots: %q", stderr)
	}
}

func TestListImplicitRootRemainsUsableWithInvalidSessionPin(t *testing.T) {
	tests := []struct {
		name           string
		configurePin   func(t *testing.T, baseRoot string)
		wantDiagnostic string
	}{
		{
			name: "incomplete legacy pin",
			configurePin: func(t *testing.T, _ string) {
				t.Setenv(envSession, "session1")
			},
			wantDiagnostic: "incomplete AMQ session pin",
		},
		{
			name: "malformed identity pin",
			configurePin: func(t *testing.T, baseRoot string) {
				t.Setenv(envSession, "session1")
				t.Setenv(envBaseRoot, baseRoot)
				t.Setenv(envRootID, "malformed-root-id")
				t.Setenv(envBaseRootID, "malformed-base-id")
			},
			wantDiagnostic: "unverifiable AMQ identity pin",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			baseRoot := filepath.Join(parent, ".agent-mail")
			targetRoot := sessionRoot(t, parent, "session1", "alice")
			deliverGuardMessage(t, targetRoot, "alice", "invalid-pin-inspection")

			clearSendMailboxTestEnv(t)
			t.Setenv(envRoot, targetRoot)
			test.configurePin(t, baseRoot)

			stdout, stderr, err := captureEnvOutput(t, func() error {
				return runList([]string{"--me", "alice", "--new", "--json"})
			})
			if err != nil {
				t.Fatalf("ordinary implicit list must stay usable: %v", err)
			}
			if !strings.Contains(stdout, `"id": "invalid-pin-inspection"`) {
				t.Fatalf("list missed active-root message: %q", stdout)
			}
			if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, test.wantDiagnostic) {
				t.Fatalf("list did not surface %q as a warning: %q", test.wantDiagnostic, stderr)
			}
			if got := inboxCount(t, targetRoot, "alice"); got != 1 {
				t.Fatalf("list mutated inbox; count = %d, want 1", got)
			}
		})
	}
}

func TestListWarnsOnImplicitGlobalRootWhenCwdHasRepoLocalSession(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global project")
	repoProject := filepath.Join(parent, "snagline project")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice")
	repoRoot := sessionRoot(t, repoProject, "session1", "alice")
	deliverGuardMessage(t, globalRoot, "alice", "global-inspection")

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	if err := os.Chdir(repoProject); err != nil {
		t.Fatal(err)
	}
	pinSendSessionForTest(t, globalBase, globalRoot, "session1")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("list remains a warning-only inspection path: %v", err)
	}
	if !strings.Contains(stdout, `"id": "global-inspection"`) {
		t.Fatalf("list did not inspect the active global root: %q", stdout)
	}
	for _, want := range []string{"warning:", "active root", globalRoot, repoRoot, "repo-local root"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("list context warning missing %q: %q", want, stderr)
		}
	}
	if !strings.Contains(stderr, "--root "+shellQuotePosix(globalRoot)) {
		t.Fatalf("list guidance does not shell-quote active root: %q", stderr)
	}
	if got := inboxCount(t, globalRoot, "alice"); got != 1 {
		t.Fatalf("list mutated global inbox; count = %d, want 1", got)
	}
	if got := inboxCount(t, repoRoot, "alice"); got != 0 {
		t.Fatalf("list touched repo-local inbox; count = %d, want 0", got)
	}
}

func TestListPinnedAMRootOverridesBrokenConfigAndWarnsOnDetectedLocalQueue(t *testing.T) {
	targetRoot := initializedSendMailboxRoot(t, "alice")
	deliverGuardMessage(t, targetRoot, "alice", "broken-config-inspection")
	projectDir := enterBrokenRootProject(t)
	localRoot := filepath.Join(projectDir, defaultCoopRoot)
	if err := fsq.EnsureAgentDirs(localRoot, "alice"); err != nil {
		t.Fatalf("initialize detectable repo-local queue: %v", err)
	}
	pinSendSessionForTest(t, targetRoot, targetRoot, "")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("list should keep AM_ROOT as a warning-only inspection path: %v", err)
	}
	if !strings.Contains(stdout, `"id": "broken-config-inspection"`) {
		t.Fatalf("list did not inspect the AM_ROOT target: %q", stdout)
	}
	for _, want := range []string{"warning:", "active root", targetRoot, localRoot, "repo-local root"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("list conflict warning missing %q: %q", want, stderr)
		}
	}
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("list mutated AM_ROOT inbox; count = %d, want 1", got)
	}
	if got := inboxCount(t, localRoot, "alice"); got != 0 {
		t.Fatalf("list touched repo-local inbox; count = %d, want 0", got)
	}
}

func TestListExplicitPinnedBaseWarnsOnContradictoryLegacyAMRoot(t *testing.T) {
	baseRoot := initializedSendMailboxRoot(t, "alice")
	if err := fsq.EnsureAgentDirs(filepath.Join(baseRoot, "current"), "alice"); err != nil {
		t.Fatalf("initialize pinned session: %v", err)
	}
	deliverGuardMessage(t, baseRoot, "alice", "base-read-only")
	foreignRoot := initializedSendMailboxRoot(t, "alice")

	t.Setenv(envRoot, foreignRoot)
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "current")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--root", baseRoot, "--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("explicit pinned-base list should remain read-only inspection: %v", err)
	}
	if !strings.Contains(stdout, `"id": "base-read-only"`) {
		t.Fatalf("list did not inspect explicit pinned base: %q", stdout)
	}
	for _, want := range []string{"warning:", "differs from pinned root", foreignRoot, filepath.Join(baseRoot, "current")} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("contradictory pinned-base warning missing %q: %q", want, stderr)
		}
	}
	if got := inboxCount(t, baseRoot, "alice"); got != 1 {
		t.Fatalf("pinned-base list mutated inbox; count = %d, want 1", got)
	}
}

func TestListHonorsUnpinnedAMRootWithoutCwdConflictWarning(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	repoProject := filepath.Join(parent, "snagline")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice")
	_ = sessionRoot(t, repoProject, "session1", "alice")

	t.Chdir(repoProject)
	clearSendMailboxTestEnv(t)
	t.Setenv(envRoot, globalRoot)

	_, stderr, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("unpinned AM_ROOT list: %v", err)
	}
	if strings.Contains(stderr, "repo-local root") {
		t.Fatalf("unpinned AM_ROOT produced cwd conflict warning: %q", stderr)
	}
}

func TestListExplicitOwnBaseRootSuppressesPinWarning(t *testing.T) {
	for _, identityPin := range []bool{false, true} {
		name := "legacy"
		if identityPin {
			name = "identity"
		}
		t.Run(name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "project with space")
			baseRoot := filepath.Join(parent, ".agent-mail")
			currentRoot := sessionRoot(t, parent, "collab", "alice")
			if err := fsq.EnsureAgentDirs(baseRoot, "alice"); err != nil {
				t.Fatalf("ensure base mailbox: %v", err)
			}
			deliverGuardMessage(t, baseRoot, "alice", "base-inspection")

			t.Setenv(envRoot, currentRoot)
			t.Setenv(envBaseRoot, baseRoot)
			t.Setenv(envSession, "collab")
			setOptionalEnv(t, envRootID, "", false)
			setOptionalEnv(t, envBaseRootID, "", false)
			if identityPin {
				rootID, err := resolveTreeIdentityToken(currentRoot)
				if err != nil {
					t.Fatalf("resolve current root identity: %v", err)
				}
				baseRootID, err := resolveTreeIdentityToken(baseRoot)
				if err != nil {
					t.Fatalf("resolve base root identity: %v", err)
				}
				t.Setenv(envRootID, rootID)
				t.Setenv(envBaseRootID, baseRootID)
			}

			stdout, stderr, err := captureEnvOutput(t, func() error {
				return runList([]string{"--root", baseRoot, "--me", "alice", "--new", "--json"})
			})
			if err != nil {
				t.Fatalf("explicit own-base list: %v", err)
			}
			if !strings.Contains(stdout, `"id": "base-inspection"`) {
				t.Fatalf("explicit own-base list missed message: %q", stdout)
			}
			if strings.Contains(stderr, "warning:") {
				t.Fatalf("explicit own-base list emitted pin warning: %q", stderr)
			}
		})
	}
}

func TestListPreservesPinWarningOutsideExplicitOwnBaseRoot(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		explicit  bool
		configure func(t *testing.T, baseRoot, currentRoot string)
	}{
		{
			name:     "implicit own base root",
			target:   "base",
			explicit: false,
		},
		{
			name:     "explicit sibling session",
			target:   "sibling",
			explicit: true,
		},
		{
			name:     "explicit foreign root",
			target:   "foreign",
			explicit: true,
		},
		{
			name:     "stale identity pin",
			target:   "base",
			explicit: true,
			configure: func(t *testing.T, baseRoot, currentRoot string) {
				t.Helper()
				rootID, err := resolveTreeIdentityToken(currentRoot)
				if err != nil {
					t.Fatalf("resolve current root identity: %v", err)
				}
				otherRoot := t.TempDir()
				staleBaseID, err := resolveTreeIdentityToken(otherRoot)
				if err != nil {
					t.Fatalf("resolve stale base identity: %v", err)
				}
				t.Setenv(envRootID, rootID)
				t.Setenv(envBaseRootID, staleBaseID)
			},
		},
		{
			name:     "malformed pin",
			target:   "base",
			explicit: true,
			configure: func(t *testing.T, _, _ string) {
				t.Helper()
				t.Setenv(envBaseRoot, "relative-base")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			baseRoot := filepath.Join(parent, ".agent-mail")
			currentRoot := sessionRoot(t, parent, "session1", "alice")
			siblingRoot := sessionRoot(t, parent, "session2", "alice")
			foreignRoot := filepath.Join(t.TempDir(), "foreign-root")
			for _, root := range []string{baseRoot, foreignRoot} {
				if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
					t.Fatalf("ensure mailbox at %s: %v", root, err)
				}
			}

			targetRoot := baseRoot
			switch test.target {
			case "sibling":
				targetRoot = siblingRoot
			case "foreign":
				targetRoot = foreignRoot
			}
			deliverGuardMessage(t, targetRoot, "alice", "warning-preserved")

			ambientRoot := currentRoot
			if !test.explicit {
				ambientRoot = targetRoot
			}
			t.Setenv(envRoot, ambientRoot)
			t.Setenv(envBaseRoot, baseRoot)
			t.Setenv(envSession, "session1")
			setOptionalEnv(t, envRootID, "", false)
			setOptionalEnv(t, envBaseRootID, "", false)
			if test.configure != nil {
				test.configure(t, baseRoot, currentRoot)
			}

			args := []string{"--me", "alice", "--new", "--json"}
			if test.explicit {
				args = append([]string{"--root", targetRoot}, args...)
			}
			stdout, stderr, err := captureEnvOutput(t, func() error {
				return runList(args)
			})
			if err != nil {
				t.Fatalf("list remains an inspection path: %v", err)
			}
			if !strings.Contains(stdout, `"id": "warning-preserved"`) {
				t.Fatalf("list missed target message: %q", stdout)
			}
			if !strings.Contains(stderr, "warning:") {
				t.Fatalf("pin warning was suppressed outside explicit own base: %q", stderr)
			}
		})
	}
}

func TestDrainExplicitOwnBaseRootStillRefusesPinMismatch(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	if err := fsq.EnsureAgentDirs(baseRoot, "alice"); err != nil {
		t.Fatalf("ensure base mailbox: %v", err)
	}
	deliverGuardMessage(t, baseRoot, "alice", "base-drain")

	t.Setenv(envRoot, currentRoot)
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "collab")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	err := runDrain([]string{"--root", baseRoot, "--me", "alice"})
	assertConsumeRefused(t, err, "drain")
	if got := inboxCount(t, baseRoot, "alice"); got != 1 {
		t.Fatalf("base inbox count = %d, want 1 untouched", got)
	}
}

func TestListDoesNotAcceptPinOverrideFlag(t *testing.T) {
	_, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--root", t.TempDir(), "--me", "alice", "--ignore-session-pin"})
	})
	if err == nil || GetExitCode(err) != ExitUsage {
		t.Fatalf("list --ignore-session-pin should remain a usage error, got %v", err)
	}
}

func TestListMissingMailboxIsNotEmpty(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	root := filepath.Join(baseRoot, "collab")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "collab")

	_, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--new"})
	})
	if err == nil || GetExitCode(err) != ExitNotFound {
		t.Fatalf("missing mailbox should be a not-found error, got %v", err)
	}
}

func TestListSessionFlagTargetsSiblingFromBaseRoot(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	siblingRoot := sessionRoot(t, parent, "session1", "alice")
	deliverGuardMessage(t, siblingRoot, "alice", "targeted")

	t.Setenv("AM_ROOT", baseRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--me", "alice", "--session", "session1", "--new", "--json"})
	})
	if err != nil {
		t.Fatalf("runList --session: %v", err)
	}
	if !strings.Contains(stdout, `"id": "targeted"`) {
		t.Fatalf("session-targeted list did not return sibling message: %q", stdout)
	}
}

func TestDoctorOpsReportsSiblingBacklogMismatch(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	siblingRoot := sessionRoot(t, parent, "session1", "alice")
	deliverGuardMessage(t, siblingRoot, "alice", "waiting-1")
	deliverGuardMessage(t, siblingRoot, "alice", "waiting-2")
	if err := os.MkdirAll(filepath.Join(baseRoot, "meta"), 0o700); err != nil {
		t.Fatalf("mkdir base meta: %v", err)
	}
	if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result := runOpsChecks(currentRoot, "env", false)
	for _, hint := range result.Hints {
		if hint.Code != "sibling_backlog" {
			continue
		}
		if hint.Status != "warn" {
			t.Fatalf("sibling backlog status = %q, want warn", hint.Status)
		}
		assertSiblingHint(t, "note: "+hint.Message, 2, "alice", "session1")
		if !strings.Contains(hint.Message, "current: collab") {
			t.Fatalf("doctor hint must identify current context: %q", hint.Message)
		}
		return
	}
	t.Fatalf("doctor ops missing sibling_backlog hint: %#v", result.Hints)
}

func TestDoctorOpsReportsBaseBacklogMismatch(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "project with space")
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	if err := fsq.EnsureAgentDirs(baseRoot, "alice"); err != nil {
		t.Fatalf("ensure base mailbox: %v", err)
	}
	deliverGuardMessage(t, baseRoot, "alice", "waiting-at-base")
	if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result := runOpsChecks(currentRoot, "env", false)
	if len(result.Agents) != 1 || result.Agents[0].Handle != "alice" || result.Agents[0].UnreadCount != 0 {
		t.Fatalf("active-session stats = %#v, want alice with zero unread", result.Agents)
	}
	var matches []opsHint
	for _, hint := range result.Hints {
		if hint.Code == "base_backlog" {
			matches = append(matches, hint)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("base_backlog hints = %#v, want exactly one", matches)
	}
	hint := matches[0]
	if hint.Status != "warn" {
		t.Fatalf("base backlog status = %q, want warn", hint.Status)
	}
	for _, want := range []string{
		`1 pending for "alice" in base root "` + baseRoot + `"`,
		"current: collab",
		"amq list --root '" + baseRoot + "' --me alice --new",
	} {
		if !strings.Contains(hint.Message, want) {
			t.Fatalf("base backlog hint missing %q: %q", want, hint.Message)
		}
	}

	encoded, err := json.Marshal(hint)
	if err != nil {
		t.Fatalf("marshal base backlog hint: %v", err)
	}
	var wire struct {
		Code    string `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Backlog *struct {
			Root           string `json:"root"`
			CurrentSession string `json:"current_session"`
			Agent          string `json:"agent"`
			Pending        int    `json:"pending"`
			Command        string `json:"command"`
		} `json:"backlog"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal base backlog hint: %v", err)
	}
	if wire.Code != hint.Code || wire.Status != hint.Status || wire.Message != hint.Message {
		t.Fatalf("existing hint contract changed: %#v", wire)
	}
	if wire.Backlog == nil {
		t.Fatalf("base backlog hint missing structured backlog: %s", encoded)
	}
	wantCommand := "amq list --root " + shellQuoteArg(baseRoot) + " --me alice --new"
	if wire.Backlog.Root != baseRoot ||
		wire.Backlog.CurrentSession != "collab" ||
		wire.Backlog.Agent != "alice" ||
		wire.Backlog.Pending != 1 ||
		wire.Backlog.Command != wantCommand {
		t.Fatalf("structured base backlog = %#v, want root=%q current_session=collab agent=alice pending=1 command=%q",
			wire.Backlog, baseRoot, wantCommand)
	}
}

func TestDoctorOpsBaseBacklogHintRequiresSessionAndPendingMessage(t *testing.T) {
	tests := []struct {
		name         string
		sessionRoot  bool
		baseEntry    string
		baseEntryDir bool
	}{
		{name: "base root is current", baseEntry: "waiting.md"},
		{name: "session with empty base", sessionRoot: true},
		{name: "session ignores dotfile", sessionRoot: true, baseEntry: ".ignored.md"},
		{name: "session ignores non-message", sessionRoot: true, baseEntry: "ignored.txt"},
		{name: "session ignores message-shaped directory", sessionRoot: true, baseEntry: "queued.md", baseEntryDir: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			baseRoot := filepath.Join(parent, ".agent-mail")
			if err := fsq.EnsureAgentDirs(baseRoot, "alice"); err != nil {
				t.Fatalf("ensure base mailbox: %v", err)
			}
			if test.baseEntry != "" {
				entryPath := filepath.Join(fsq.AgentInboxNew(baseRoot, "alice"), test.baseEntry)
				if test.baseEntryDir {
					if err := os.Mkdir(entryPath, 0o700); err != nil {
						t.Fatalf("mkdir base entry: %v", err)
					}
				} else if err := os.WriteFile(entryPath, []byte("test"), 0o600); err != nil {
					t.Fatalf("write base entry: %v", err)
				}
			}
			if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), config.Config{
				Version: 1,
				Agents:  []string{"alice"},
			}, true); err != nil {
				t.Fatalf("write config: %v", err)
			}

			root := baseRoot
			if test.sessionRoot {
				root = sessionRoot(t, parent, "collab", "alice")
			}
			result := runOpsChecks(root, "env", false)
			for _, hint := range result.Hints {
				if hint.Code == "base_backlog" {
					t.Fatalf("unexpected base_backlog hint: %#v", hint)
				}
			}
		})
	}
}

func TestDoctorOpsBaseBacklogHintIgnoresInvalidBaseInbox(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	if err := os.MkdirAll(filepath.Join(baseRoot, "meta"), 0o700); err != nil {
		t.Fatalf("mkdir base meta: %v", err)
	}
	if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "agents"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write invalid base agents path: %v", err)
	}

	result := runOpsChecks(currentRoot, "env", false)
	for _, hint := range result.Hints {
		if hint.Code == "base_backlog" {
			t.Fatalf("invalid base inbox produced base_backlog hint: %#v", hint)
		}
	}
}

func TestDoctorOpsBaseBacklogHintRequiresAuthoritativeSessionClassification(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), "custom-root")
	currentRoot := filepath.Join(baseRoot, "collab")
	siblingRoot := filepath.Join(baseRoot, "session1")
	for _, root := range []string{currentRoot, siblingRoot} {
		if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
			t.Fatalf("ensure session mailbox at %s: %v", root, err)
		}
	}
	if err := fsq.EnsureAgentDirs(baseRoot, "alice"); err != nil {
		t.Fatalf("ensure base mailbox: %v", err)
	}
	deliverGuardMessage(t, baseRoot, "alice", "waiting-at-ambiguous-base")
	if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result := runOpsChecks(currentRoot, "env", false)
	for _, hint := range result.Hints {
		if hint.Code == "base_backlog" {
			t.Fatalf("heuristic-only custom layout produced base_backlog hint: %#v", hint)
		}
	}
}

func TestDoctorOpsBaseBacklogHintRejectsSymlinkSession(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	realRoot := filepath.Join(parent, "real-session")
	if err := fsq.EnsureAgentDirs(realRoot, "alice"); err != nil {
		t.Fatalf("ensure real session mailbox: %v", err)
	}
	currentRoot := filepath.Join(baseRoot, "collab")
	if err := os.MkdirAll(baseRoot, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.Symlink(realRoot, currentRoot); err != nil {
		t.Skipf("symlink session unsupported: %v", err)
	}
	if err := fsq.EnsureAgentDirs(baseRoot, "alice"); err != nil {
		t.Fatalf("ensure base mailbox: %v", err)
	}
	deliverGuardMessage(t, baseRoot, "alice", "waiting-at-base")
	if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result := runOpsChecks(currentRoot, "env", false)
	for _, hint := range result.Hints {
		if hint.Code == "base_backlog" {
			t.Fatalf("symlink session produced base_backlog hint: %#v", hint)
		}
	}
}

func TestDoctorOpsBaseBacklogHintRejectsEscapingMailboxSymlink(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	currentRoot := sessionRoot(t, parent, "collab", "alice")
	if err := fsq.EnsureRootDirs(baseRoot); err != nil {
		t.Fatalf("ensure base root: %v", err)
	}
	if err := config.WriteConfig(filepath.Join(baseRoot, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	outsideAgent := filepath.Join(parent, "outside-alice")
	outsideInbox := filepath.Join(outsideAgent, "inbox", "new")
	if err := os.MkdirAll(outsideInbox, 0o700); err != nil {
		t.Fatalf("mkdir outside inbox: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideInbox, "outside.md"), []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside message: %v", err)
	}
	if err := os.Symlink(outsideAgent, filepath.Join(baseRoot, "agents", "alice")); err != nil {
		t.Skipf("mailbox symlink unsupported: %v", err)
	}

	result := runOpsChecks(currentRoot, "env", false)
	for _, hint := range result.Hints {
		if hint.Code == "base_backlog" {
			t.Fatalf("escaping mailbox symlink produced base_backlog hint: %#v", hint)
		}
	}
}

func assertSiblingHint(t *testing.T, stderr string, count int, handle, session string) {
	t.Helper()
	wantSummary := strings.Join([]string{
		"note:",
		strconv.Itoa(count),
		"pending for \"" + handle + "\"",
		"in sibling session \"" + session + "\"",
	}, " ")
	if !strings.Contains(stderr, wantSummary) {
		t.Fatalf("stderr missing sibling summary %q: %q", wantSummary, stderr)
	}
	wantCommand := "amq list --session " + session + " --me " + handle + " --new"
	if !strings.Contains(stderr, wantCommand) {
		t.Fatalf("stderr missing exact inspection command %q: %q", wantCommand, stderr)
	}
}

func assertBaseHint(t *testing.T, stderr string, count int, handle, baseRoot, session string) {
	t.Helper()
	wantSummary := strings.Join([]string{
		"note:",
		strconv.Itoa(count),
		"pending for \"" + handle + "\"",
		"in base root \"" + baseRoot + "\"",
		"(current: " + session + ")",
	}, " ")
	if !strings.Contains(stderr, wantSummary) {
		t.Fatalf("stderr missing base summary %q: %q", wantSummary, stderr)
	}
	wantCommand := "amq list --root " + shellQuoteArg(baseRoot) + " --me " + handle + " --new"
	if !strings.Contains(stderr, wantCommand) {
		t.Fatalf("stderr missing exact base inspection command %q: %q", wantCommand, stderr)
	}
}
