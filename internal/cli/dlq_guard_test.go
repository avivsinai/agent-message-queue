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

func TestDLQReadRefusesImplicitGlobalRootWithoutInspectingForeignEnvelope(t *testing.T) {
	fixture := preparePinnedDLQConflictFixture(t, "read")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{
			"--me", "alice",
			"--id", fixture.dlqID,
			"--json",
		})
	})
	assertConsumeRefused(t, err, "dlq read")
	if stdout != "" {
		t.Fatalf("refused DLQ read emitted stdout: %q", stdout)
	}

	assertPinnedDLQConflictFixtureUnchanged(t, fixture)
	if _, err := os.Stat(filepath.Join(fsq.AgentDLQCur(fixture.globalRoot, "alice"), fixture.dlqFilename)); !os.IsNotExist(err) {
		t.Fatalf("refused DLQ read moved foreign envelope to cur: %v", err)
	}
}

func TestDLQRetryRefusesImplicitGlobalRootWithoutRedeliveringForeignEnvelope(t *testing.T) {
	fixture := preparePinnedDLQConflictFixture(t, "retry")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{
			"--me", "alice",
			"--id", fixture.dlqID,
			"--json",
		})
	})
	assertConsumeRefused(t, err, "dlq retry")
	if stdout != "" {
		t.Fatalf("refused DLQ retry emitted stdout: %q", stdout)
	}

	assertPinnedDLQConflictFixtureUnchanged(t, fixture)
	for _, path := range []string{
		filepath.Join(fsq.AgentInboxNew(fixture.globalRoot, "alice"), fixture.originalFilename),
		filepath.Join(fsq.AgentInboxCur(fixture.globalRoot, "alice"), fixture.originalFilename),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refused DLQ retry redelivered foreign payload at %s: %v", path, err)
		}
	}
}

func TestDLQPurgeRefusesImplicitGlobalRootWithoutDeletingForeignEnvelope(t *testing.T) {
	fixture := preparePinnedDLQConflictFixture(t, "purge")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{
			"--me", "alice",
			"--yes",
			"--json",
		})
	})
	assertConsumeRefused(t, err, "dlq purge")
	if stdout != "" {
		t.Fatalf("refused DLQ purge emitted stdout: %q", stdout)
	}

	assertPinnedDLQConflictFixtureUnchanged(t, fixture)
}

func TestDLQListGuardsContextBeforeStrictConfigValidation(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	repoProject := filepath.Join(parent, "snagline")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice")
	_ = sessionRoot(t, repoProject, "session1", "alice")
	if err := os.WriteFile(filepath.Join(globalRoot, "meta", "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt global config: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	if err := os.Chdir(repoProject); err != nil {
		t.Fatal(err)
	}
	pinSendSessionForTest(t, globalBase, globalRoot, "session1")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--me", "alice", "--strict", "--json"})
	})
	assertConsumeRefused(t, err, "dlq list")
	if stdout != "" {
		t.Fatalf("refused DLQ list emitted stdout: %q", stdout)
	}
	if strings.Contains(err.Error(), "config.json") {
		t.Fatalf("DLQ list validated foreign config before context guard: %v", err)
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

func TestDLQListSelectedMissingDirectoryIsNotReportedAsEmpty(t *testing.T) {
	tests := []struct {
		name string
		flag string
		dir  func(string, string) string
	}{
		{name: "new", flag: "--new", dir: fsq.AgentDLQNew},
		{name: "cur", flag: "--cur", dir: fsq.AgentDLQCur},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := initializedSendMailboxRoot(t, "alice")
			missing := tc.dir(root, "alice")
			if err := os.Remove(missing); err != nil {
				t.Fatalf("remove selected DLQ directory: %v", err)
			}

			stdout, _, err := captureEnvOutput(t, func() error {
				return runDLQList([]string{"--root", root, "--me", "alice", tc.flag, "--json"})
			})
			if err == nil || GetExitCode(err) != ExitNotFound {
				t.Fatalf("missing selected DLQ directory should be not-found, got %v", err)
			}
			if stdout != "" {
				t.Fatalf("missing selected DLQ directory was reported as empty: %q", stdout)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Fatalf("missing selected DLQ directory error does not name %s: %v", missing, err)
			}
		})
	}
}

func TestDLQListSelectedBoxIgnoresUnrelatedMissingMailboxLeaves(t *testing.T) {
	tests := []struct {
		name       string
		flag       string
		moveToCur  bool
		removeDirs func(root string) []string
	}{
		{
			name: "new",
			flag: "--new",
			removeDirs: func(root string) []string {
				return []string{
					fsq.AgentInboxNew(root, "alice"),
					fsq.AgentInboxCur(root, "alice"),
					fsq.AgentDLQCur(root, "alice"),
				}
			},
		},
		{
			name:      "cur",
			flag:      "--cur",
			moveToCur: true,
			removeDirs: func(root string) []string {
				return []string{
					fsq.AgentInboxNew(root, "alice"),
					fsq.AgentInboxCur(root, "alice"),
					fsq.AgentDLQNew(root, "alice"),
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := initializedSendMailboxRoot(t, "alice")
			originalID := "selected-" + tc.name
			deliverGuardMessage(t, root, "alice", originalID)
			deliveryRoot := openDeliveryRootForCLITest(t, root)
			dlqPath, err := fsq.MoveToDLQ(
				deliveryRoot,
				"alice",
				originalID+".md",
				originalID,
				"selected_box",
				"unrelated mailbox leaves must not block inspection",
			)
			if err != nil {
				t.Fatalf("MoveToDLQ: %v", err)
			}
			if tc.moveToCur {
				if err := fsq.MoveDLQNewToCur(deliveryRoot, "alice", filepath.Base(dlqPath)); err != nil {
					t.Fatalf("MoveDLQNewToCur: %v", err)
				}
			}
			if err := deliveryRoot.Close(); err != nil {
				t.Fatalf("close delivery root: %v", err)
			}

			for _, dir := range tc.removeDirs(root) {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatalf("remove unrelated mailbox leaf %s: %v", dir, err)
				}
			}

			stdout, _, err := captureEnvOutput(t, func() error {
				return runDLQList([]string{"--root", root, "--me", "alice", tc.flag, "--json"})
			})
			if err != nil {
				t.Fatalf("list selected %s box with unrelated leaves missing: %v", tc.name, err)
			}
			if !strings.Contains(stdout, originalID) || !strings.Contains(stdout, `"box": "`+tc.name+`"`) {
				t.Fatalf("selected %s DLQ item missing from output: %q", tc.name, stdout)
			}
		})
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

func TestDLQListAllowsExplicitRootWithPinOverride(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	pinnedRoot := sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	pinSendSessionForTest(t, baseRoot, pinnedRoot, "session1")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{
			"--root", targetRoot,
			"--ignore-session-pin",
			"--me", "alice",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("explicit DLQ list override: %v", err)
	}
	if strings.TrimSpace(stdout) != "null" {
		t.Fatalf("explicit DLQ list override output = %q, want null", stdout)
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

type pinnedDLQConflictFixture struct {
	globalRoot       string
	dlqPath          string
	dlqFilename      string
	dlqID            string
	originalFilename string
	beforeState      map[string][]byte
}

func preparePinnedDLQConflictFixture(t *testing.T, operation string) pinnedDLQConflictFixture {
	t.Helper()
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	repoProject := filepath.Join(parent, "snagline")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice")
	_ = sessionRoot(t, repoProject, "session1", "alice")

	originalID := "foreign-dlq-" + operation
	originalFilename := originalID + ".md"
	deliverGuardMessage(t, globalRoot, "alice", originalID)
	dlqPath, err := fsq.MoveToDLQ(
		openDeliveryRootForCLITest(t, globalRoot),
		"alice",
		originalFilename,
		originalID,
		"foreign_root_"+operation+"_sentinel",
		"must remain unchanged from the repo-local cwd",
	)
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}
	dlqFilename := filepath.Base(dlqPath)
	fixture := pinnedDLQConflictFixture{
		globalRoot:       globalRoot,
		dlqPath:          dlqPath,
		dlqFilename:      dlqFilename,
		dlqID:            strings.TrimSuffix(dlqFilename, ".md"),
		originalFilename: originalFilename,
		beforeState:      snapshotDLQFileState(t, globalRoot, "alice"),
	}

	t.Chdir(repoProject)
	pinSendSessionForTest(t, globalBase, globalRoot, "session1")
	return fixture
}

func assertPinnedDLQConflictFixtureUnchanged(t *testing.T, fixture pinnedDLQConflictFixture) {
	t.Helper()
	afterState := snapshotDLQFileState(t, fixture.globalRoot, "alice")
	if !reflect.DeepEqual(afterState, fixture.beforeState) {
		t.Fatalf("refused DLQ command changed foreign DLQ state: before=%v after=%v", fixture.beforeState, afterState)
	}
	if _, err := os.Stat(fixture.dlqPath); err != nil {
		t.Fatalf("foreign DLQ sentinel missing after refusal: %v", err)
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

func TestDLQListSkipsDotfilesEvenWhenTheyParse(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice")
	visible := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "visible-dlq")
	data, err := os.ReadFile(visible)
	if err != nil {
		t.Fatalf("read visible DLQ envelope: %v", err)
	}
	hidden := filepath.Join(fsq.AgentDLQNew(root, "alice"), ".hidden.md")
	if err := os.WriteFile(hidden, data, 0o600); err != nil {
		t.Fatalf("write hidden DLQ envelope: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--root", root, "--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("dlq list: %v", err)
	}
	var items []dlqListItem
	if err := unmarshalJSONOutput(stdout, &items); err != nil {
		t.Fatalf("decode dlq list: %v (output: %s)", err, stdout)
	}
	wantID := strings.TrimSuffix(filepath.Base(visible), ".md")
	if len(items) != 1 || items[0].ID != wantID {
		t.Fatalf("dlq list items = %#v, want only %s (dotfile skipped)", items, wantID)
	}
}

func TestDLQListSortsByFailureTimeWithoutTreatingZeroSortKeyAsNewest(t *testing.T) {
	t.Run("parsed newest first", func(t *testing.T) {
		root := initializedSendMailboxRoot(t, "alice")
		writeDLQEnvelopeWithFailureTime(t, root, "alice", "older-parsed.md", "2020-01-01T00:00:00Z")
		writeDLQEnvelopeWithFailureTime(t, root, "alice", "newer-parsed.md", "2024-06-01T00:00:00Z")
		stdout, _, err := captureEnvOutput(t, func() error {
			return runDLQList([]string{"--root", root, "--me", "alice", "--new", "--json"})
		})
		if err != nil {
			t.Fatalf("dlq list: %v", err)
		}
		var items []dlqListItem
		if err := unmarshalJSONOutput(stdout, &items); err != nil {
			t.Fatalf("decode: %v (output: %s)", err, stdout)
		}
		if len(items) != 2 || items[0].ID != "newer-parsed" || items[1].ID != "older-parsed" {
			t.Fatalf("parsed order = %v, want newer-parsed then older-parsed", idsOf(items))
		}
	})

	t.Run("unparsed string order beats zero SortKey", func(t *testing.T) {
		root := initializedSendMailboxRoot(t, "alice")
		writeDLQEnvelopeWithFailureTime(t, root, "alice", "parsed.md", "2020-01-01T00:00:00Z")
		writeDLQEnvelopeWithFailureTime(t, root, "alice", "unparsed.md", "not-a-rfc3339-time")
		stdout, _, err := captureEnvOutput(t, func() error {
			return runDLQList([]string{"--root", root, "--me", "alice", "--new", "--json"})
		})
		if err != nil {
			t.Fatalf("dlq list: %v", err)
		}
		var items []dlqListItem
		if err := unmarshalJSONOutput(stdout, &items); err != nil {
			t.Fatalf("decode: %v (output: %s)", err, stdout)
		}
		if len(items) != 2 || items[0].ID != "unparsed" || items[1].ID != "parsed" {
			t.Fatalf("mixed order = %v, want unparsed then parsed (string compare, not zero-time After)", idsOf(items))
		}
	})
}

func idsOf(items []dlqListItem) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}
