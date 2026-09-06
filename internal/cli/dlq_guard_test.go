package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDLQListRefusesImplicitGlobalRootBeforeReadingForeignDLQ(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	repoProject := filepath.Join(parent, "snagline")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice")
	repoRoot := sessionRoot(t, repoProject, "session1", "alice")

	const originalID = "distinctive-foreign-dlq"
	deliverGuardMessage(t, globalRoot, "alice", originalID)
	dlqPath, err := fsq.MoveToDLQ(
		openDeliveryRootForCLITest(t, globalRoot),
		"alice",
		originalID+".md",
		originalID,
		"foreign_root_sentinel",
		"must not be listed from the repo-local cwd",
	)
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}
	beforeBytes, err := os.ReadFile(dlqPath)
	if err != nil {
		t.Fatalf("read DLQ before list: %v", err)
	}
	beforeState := snapshotDLQFileState(t, globalRoot, "alice")

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
		return runDLQList([]string{"--me", "alice", "--json"})
	})
	assertConsumeRefused(t, err, "dlq list")
	if stdout != "" {
		t.Fatalf("refused DLQ list emitted stdout: %q", stdout)
	}
	for _, want := range []string{"active root", globalRoot, repoRoot, "repo-local root"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("DLQ context refusal missing %q: %v", want, err)
		}
	}
	if strings.Contains(stdout+stderr, "foreign_root_sentinel") ||
		strings.Contains(stdout+stderr, originalID) {
		t.Fatalf("refused DLQ list exposed foreign DLQ content: stdout=%q stderr=%q", stdout, stderr)
	}
	afterBytes, err := os.ReadFile(dlqPath)
	if err != nil {
		t.Fatalf("read DLQ after refusal: %v", err)
	}
	if !bytes.Equal(afterBytes, beforeBytes) {
		t.Fatal("refused DLQ list changed foreign envelope bytes")
	}
	if afterState := snapshotDLQFileState(t, globalRoot, "alice"); !reflect.DeepEqual(afterState, beforeState) {
		t.Fatalf("refused DLQ list changed foreign DLQ state: before=%v after=%v", beforeState, afterState)
	}
}

func TestDLQListMissingMailboxIsNotReportedAsEmpty(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := t.TempDir()
	configureSendTestRoot(t, root, "alice")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--root", root, "--me", "alice", "--json"})
	})
	if err == nil || GetExitCode(err) != ExitNotFound {
		t.Fatalf("missing DLQ mailbox should be not-found, got %v", err)
	}
	if stdout != "" {
		t.Fatalf("missing DLQ mailbox was reported as empty: %q", stdout)
	}
	if !strings.Contains(err.Error(), `mailbox for "alice" is missing`) {
		t.Fatalf("missing DLQ mailbox error is not actionable: %v", err)
	}
}

func TestCollectDLQListItemsRefusesOpenedRootPathReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("renaming an open directory is not supported on Windows")
	}

	parent := t.TempDir()
	authorizedRoot := filepath.Join(parent, "authorized")
	replacementRoot := filepath.Join(parent, "replacement")
	parkedRoot := filepath.Join(parent, "authorized-parked")
	for _, fixture := range []struct {
		root   string
		id     string
		detail string
	}{
		{root: authorizedRoot, id: "authorized-dlq", detail: "authorized-content"},
		{root: replacementRoot, id: "replacement-dlq", detail: "replacement-content"},
	} {
		if err := fsq.EnsureAgentDirs(fixture.root, "alice"); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", fixture.root, err)
		}
		configureSendTestRoot(t, fixture.root, "alice")
		deliverGuardMessage(t, fixture.root, "alice", fixture.id)
		if _, err := fsq.MoveToDLQ(
			openDeliveryRootForCLITest(t, fixture.root),
			"alice",
			fixture.id+".md",
			fixture.id,
			"root_swap",
			fixture.detail,
		); err != nil {
			t.Fatalf("MoveToDLQ(%s): %v", fixture.root, err)
		}
	}

	authorized := openDeliveryRootForCLITest(t, authorizedRoot)
	if err := os.Rename(authorizedRoot, parkedRoot); err != nil {
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Rename(replacementRoot, authorizedRoot); err != nil {
		t.Fatalf("replace authorized root path: %v", err)
	}

	items, err := collectDLQListItems(authorized, "alice", []string{"new"})
	if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
		t.Fatalf("collect after root replacement error = %v, want delivery-root refusal", err)
	}
	if len(items) != 0 {
		t.Fatalf("refused root replacement returned DLQ contents: %#v", items)
	}
}

func TestDLQListSessionTargetsSibling(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	pinnedRoot := sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	pinSendSessionForTest(t, baseRoot, pinnedRoot, "session1")

	const originalID = "sibling-dlq"
	deliverGuardMessage(t, targetRoot, "alice", originalID)
	if _, err := fsq.MoveToDLQ(
		openDeliveryRootForCLITest(t, targetRoot),
		"alice",
		originalID+".md",
		originalID,
		"sibling_marker",
		"listed through deliberate session routing",
	); err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--session", "session2", "--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("sibling DLQ list: %v", err)
	}
	if !strings.Contains(stdout, originalID) || !strings.Contains(stdout, "sibling_marker") {
		t.Fatalf("sibling DLQ item missing from output: %q", stdout)
	}
}

func snapshotDLQFileState(t *testing.T, root, agent string) map[string][]byte {
	t.Helper()
	state := make(map[string][]byte)
	for _, box := range []string{"new", "cur"} {
		dir := filepath.Join(root, "agents", agent, "dlq", box)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			state[filepath.Join(box, entry.Name())] = data
		}
	}
	return state
}
