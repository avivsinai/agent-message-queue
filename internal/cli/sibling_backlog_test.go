package cli

import (
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
