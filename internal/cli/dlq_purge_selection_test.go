package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDLQPurgeOutputsCommittedRemovalsBeforeJoinedSelectionAndSyncErrors(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	validPath := writeOldValidDLQEnvelope(t, root, "alice", "old-valid.md")
	corruptPath, corruptBefore := writeFreshCorruptDLQEnvelope(t, root, "alice", "fresh-corrupt-mixed.md")

	syncErr := errors.New("injected DLQ purge directory sync failure")
	oldSync := syncDLQPurgeDir
	syncDLQPurgeDir = func(deliveryRoot *fsq.DeliveryRoot, dir string) error {
		if dir == filepath.Join("agents", "alice", "dlq", "new") {
			return syncErr
		}
		return oldSync(deliveryRoot, dir)
	}
	t.Cleanup(func() { syncDLQPurgeDir = oldSync })

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{
			"--root", root,
			"--me", "alice",
			"--older-than", "24h",
			"--yes",
			"--json",
		})
	})

	var result struct {
		Removed int `json:"removed"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode purge result: %v (output: %s)", decodeErr, stdout)
	}
	if result.Removed != 1 {
		t.Fatalf("purge removed = %d, want committed removal output before error", result.Removed)
	}
	if !errors.Is(err, syncErr) {
		t.Fatalf("purge error = %T %v, want sync error", err, err)
	}
	for _, want := range []string{filepath.Base(corruptPath), "missing frontmatter start"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("purge error = %q, want joined selection detail %q", err, want)
		}
	}
	if _, statErr := os.Stat(validPath); !os.IsNotExist(statErr) {
		t.Fatalf("safely selected envelope was not removed: %v", statErr)
	}
	assertDLQEnvelopePreserved(t, corruptPath, corruptBefore)
}

func TestDLQPurgeOutputsCommittedRemovalsBeforeCandidateRemovalError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	retainedPath := writeOldValidDLQEnvelope(t, root, "alice", "a-retain.md")
	retainedBefore, readErr := os.ReadFile(retainedPath)
	if readErr != nil {
		t.Fatalf("read retained candidate fixture: %v", readErr)
	}
	removedNewPath := writeOldValidDLQEnvelope(t, root, "alice", "b-remove.md")
	removedPath := filepath.Join(fsq.AgentDLQCur(root, "alice"), filepath.Base(removedNewPath))
	if err := os.Rename(removedNewPath, removedPath); err != nil {
		t.Fatalf("move successful candidate to dlq/cur: %v", err)
	}

	injectedErr := errors.New("injected DLQ purge unlink failure")
	oldRemove := removeDLQPurgeCandidate
	removeDLQPurgeCandidate = func(deliveryRoot *fsq.DeliveryRoot, path string) error {
		if filepath.Base(path) == filepath.Base(retainedPath) {
			return injectedErr
		}
		return oldRemove(deliveryRoot, path)
	}
	t.Cleanup(func() { removeDLQPurgeCandidate = oldRemove })

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{
			"--root", root,
			"--me", "alice",
			"--older-than", "24h",
			"--yes",
			"--json",
		})
	})

	var result struct {
		Removed int `json:"removed"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode purge result: %v (output: %s)", decodeErr, stdout)
	}
	if result.Removed != 1 {
		t.Fatalf("purge removed = %d, want committed removal output before unlink error", result.Removed)
	}
	if !errors.Is(err, injectedErr) || GetExitCode(err) != ExitError {
		t.Fatalf("purge error = %T %v (exit %d), want injected non-zero unlink error", err, err, GetExitCode(err))
	}
	if !strings.Contains(err.Error(), retainedPath) {
		t.Fatalf("purge error = %q, want retained candidate path %q", err, retainedPath)
	}
	if _, statErr := os.Stat(removedPath); !os.IsNotExist(statErr) {
		t.Fatalf("successful candidate was not removed: %v", statErr)
	}
	assertDLQEnvelopePreserved(t, retainedPath, retainedBefore)
}

func TestDLQPurgeIgnoresConcurrentCandidateDisappearance(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	path := writeOldValidDLQEnvelope(t, root, "alice", "concurrent-disappearance.md")

	oldRemove := removeDLQPurgeCandidate
	removeDLQPurgeCandidate = func(deliveryRoot *fsq.DeliveryRoot, candidate string) error {
		if filepath.Base(candidate) != filepath.Base(path) {
			return oldRemove(deliveryRoot, candidate)
		}
		if err := oldRemove(deliveryRoot, candidate); err != nil {
			return err
		}
		return os.ErrNotExist
	}
	t.Cleanup(func() { removeDLQPurgeCandidate = oldRemove })

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{
			"--root", root,
			"--me", "alice",
			"--older-than", "24h",
			"--yes",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("purge concurrent disappearance: %v", err)
	}
	var result struct {
		Removed int `json:"removed"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode purge result: %v (output: %s)", decodeErr, stdout)
	}
	if result.Removed != 0 {
		t.Fatalf("purge removed = %d, want concurrent disappearance excluded from committed count", result.Removed)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("concurrently disappeared candidate remains: %v", statErr)
	}
}

func TestDLQPurgeOlderThanPreservesFreshCorruptEnvelopeAndReportsSelectionError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	path, before := writeFreshCorruptDLQEnvelope(t, root, "alice", "fresh-corrupt-purge.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{
			"--root", root,
			"--me", "alice",
			"--older-than", "24h",
			"--yes",
			"--json",
		})
	})

	var result struct {
		Removed int `json:"removed"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode purge result: %v (output: %s)", decodeErr, stdout)
	}
	if result.Removed != 0 {
		t.Errorf("purge removed = %d, want 0 when age selection is incomplete", result.Removed)
	}
	assertDLQPurgeSelectionError(t, err, filepath.Base(path))
	assertDLQEnvelopePreserved(t, path, before)
}

func TestDLQPurgeDryRunOlderThanPreservesFreshCorruptEnvelopeAndReportsSelectionError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	path, before := writeFreshCorruptDLQEnvelope(t, root, "alice", "fresh-corrupt-dry-run.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{
			"--root", root,
			"--me", "alice",
			"--older-than", "24h",
			"--dry-run",
			"--json",
		})
	})

	var result struct {
		Candidates []string `json:"candidates"`
		Count      int      `json:"count"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode dry-run purge result: %v (output: %s)", decodeErr, stdout)
	}
	if result.Count != 0 || len(result.Candidates) != 0 {
		t.Errorf("dry-run selected count = %d candidates = %q, want incomplete selection reported with no candidates", result.Count, result.Candidates)
	}
	assertDLQPurgeSelectionError(t, err, filepath.Base(path))
	assertDLQEnvelopePreserved(t, path, before)
}

func TestDLQPurgeDryRunDoesNotCreateDLQLocks(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	writeOldValidDLQEnvelope(t, root, "alice", "dry-run-manifest.md")
	before := dlqTreeManifest(t, root)
	if _, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{"--root", root, "--me", "alice", "--dry-run", "--json"})
	}); err != nil {
		t.Fatalf("dry-run purge: %v", err)
	}
	if after := dlqTreeManifest(t, root); after != before {
		t.Fatalf("dry-run changed mailbox tree:\n before %s\n after  %s", before, after)
	}
}

func TestDLQPurgeWithoutAgeFilterRemovesCorruptEnvelope(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	path, _ := writeFreshCorruptDLQEnvelope(t, root, "alice", "corrupt-unconditional-purge.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{"--root", root, "--me", "alice", "--yes", "--json"})
	})
	if err != nil {
		t.Fatalf("unconditional purge of corrupt envelope: %v", err)
	}
	var result struct {
		Removed int `json:"removed"`
	}
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("decode unconditional purge output: %v (output: %s)", err, stdout)
	}
	if result.Removed != 1 {
		t.Fatalf("unconditional purge removed = %d, want 1", result.Removed)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt envelope remains after unconditional purge: %v", err)
	}
}

func dlqTreeManifest(t *testing.T, root string) string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel+":"+entry.Type().String())
		return nil
	}); err != nil {
		t.Fatalf("walk mailbox tree: %v", err)
	}
	return strings.Join(paths, "\n")
}

func TestDLQPurgeFreshInvocationRemovesCompletedRetryAudit(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "purge-after-retry")
	env, body, err := fsq.ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("read fixture envelope: %v", err)
	}
	env.FailureTime = time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	header, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal old fixture envelope: %v", err)
	}
	if err := os.WriteFile(dlqPath, append(append([]byte("---\n"), header...), append([]byte("\n---\n"), body...)...), 0o600); err != nil {
		t.Fatalf("write old fixture envelope: %v", err)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{"--root", root, "--me", "alice", "--id", strings.TrimSuffix(filepath.Base(dlqPath), ".md")})
	}); err != nil {
		t.Fatalf("retry fixture: %v", err)
	}
	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{"--root", root, "--me", "alice", "--older-than", "24h", "--yes", "--json"})
	})
	if err != nil {
		t.Fatalf("purge completed retry audit: %v", err)
	}
	var result struct {
		Removed int `json:"removed"`
	}
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("decode purge result: %v (output %s)", err, stdout)
	}
	if result.Removed != 1 {
		t.Fatalf("purge removed %d completed retry audits, want 1", result.Removed)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentDLQCur(root, "alice"), filepath.Base(dlqPath))); !os.IsNotExist(err) {
		t.Fatalf("completed retry audit remains: %v", err)
	}
}

func TestDLQPurgeStaleCandidateCannotDeleteConcurrentRetryAudit(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "retry-purge-race")
	dlqID := strings.TrimSuffix(filepath.Base(dlqPath), ".md")
	if _, _, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{"--root", root, "--me", "alice", "--id", dlqID, "--json"})
	}); err != nil {
		t.Fatalf("move fixture to cur: %v", err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatalf("snapshot root: %v", err)
	}
	purgeRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = purgeRoot.Close() }()
	curPath := filepath.Join("agents", "alice", "dlq", "cur", filepath.Base(dlqPath))
	candidate, selected, err := selectDLQPurgeCandidate(purgeRoot, curPath, time.Time{})
	if err != nil || !selected {
		t.Fatalf("select stale purge candidate = (%#v,%t,%v)", candidate, selected, err)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{"--root", root, "--me", "alice", "--id", dlqID, "--json"})
	}); err != nil {
		t.Fatalf("concurrent retry winner: %v", err)
	}
	removed, err := removeSelectedDLQPurgeCandidate(purgeRoot, "alice", candidate)
	if err != nil || removed {
		t.Fatalf("stale purge removal = removed:%t err:%v, want no deletion", removed, err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentDLQCur(root, "alice"), filepath.Base(dlqPath))); err != nil {
		t.Fatalf("retry audit lost to stale purge: %v", err)
	}
}

func writeOldValidDLQEnvelope(t *testing.T, root, agent, filename string) string {
	t.Helper()
	env := fsq.DLQEnvelope{
		Schema:        fsq.DLQSchemaVersion,
		ID:            strings.TrimSuffix(filename, ".md"),
		OriginalID:    "original-" + strings.TrimSuffix(filename, ".md"),
		OriginalFile:  "original.md",
		FailureReason: "test_failure",
		FailureDetail: "old deterministic fixture",
		FailureTime:   time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
		SourceDir:     fsq.BoxNew,
	}
	header, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal valid DLQ envelope: %v", err)
	}
	data := append([]byte("---\n"), header...)
	data = append(data, []byte("\n---\nfixture body")...)
	path := filepath.Join(fsq.AgentDLQNew(root, agent), filename)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write valid old DLQ envelope: %v", err)
	}
	return path
}

func writeFreshCorruptDLQEnvelope(t *testing.T, root, agent, filename string) (string, []byte) {
	t.Helper()
	data := []byte("newly-created corrupt DLQ envelope")
	path := filepath.Join(fsq.AgentDLQNew(root, agent), filename)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write corrupt DLQ envelope: %v", err)
	}
	return path, data
}

func assertDLQPurgeSelectionError(t *testing.T, err error, filename string) {
	t.Helper()
	if err == nil || GetExitCode(err) != ExitError {
		t.Errorf("purge selection error = %T %v (exit %d), want non-zero ordinary error", err, err, GetExitCode(err))
		return
	}
	for _, want := range []string{filename, "missing frontmatter start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("purge selection error = %q, want detail %q", err, want)
		}
	}
}

func assertDLQEnvelopePreserved(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("corrupt DLQ envelope was not preserved at %s: %v", path, err)
		return
	}
	if !bytes.Equal(after, before) {
		t.Errorf("corrupt DLQ envelope changed: before=%q after=%q", before, after)
	}
}
