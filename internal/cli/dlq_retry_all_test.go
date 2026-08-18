package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunDLQRetrySingleTreatsProvenDeliveredAuditAsSuccess(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[jsonOutput], func(t *testing.T) {
			root := initializedSendMailboxRoot(t, "alice", "bob")
			oldRetry := retryDLQMessage
			retryDLQMessage = func(*fsq.DeliveryRoot, string, string, bool) error {
				return fsq.ErrDLQRetryDelivered
			}
			t.Cleanup(func() { retryDLQMessage = oldRetry })

			args := []string{"--root", root, "--me", "alice", "--id", "legacy-visible"}
			if jsonOutput {
				args = append(args, "--json")
			}
			stdout, _, err := captureEnvOutput(t, func() error {
				return runDLQRetry(args)
			})
			if err != nil {
				t.Fatalf("proven-delivered single retry = %v, want success", err)
			}
			if jsonOutput {
				var result struct {
					AlreadyDelivered string `json:"already_delivered"`
					AuditFinalized   bool   `json:"audit_finalized"`
				}
				if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
					t.Fatalf("decode proven-delivered output: %v (output: %s)", decodeErr, stdout)
				}
				if result.AlreadyDelivered != "legacy-visible" || !result.AuditFinalized {
					t.Fatalf("proven-delivered JSON = %#v", result)
				}
			} else if stdout != "Already delivered: legacy-visible (audit finalized).\n" {
				t.Fatalf("proven-delivered text = %q", stdout)
			}
		})
	}
}

func TestRetryAllAndSingleSerializeOneEnvelope(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "all-single-race")
	filename := filepath.Base(dlqPath)
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatalf("snapshot root: %v", err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = deliveryRoot.Close() }()

	oldRetry := retryDLQMessage
	entered := make(chan struct{})
	releaseAll := make(chan struct{})
	var once sync.Once
	retryDLQMessage = func(root *fsq.DeliveryRoot, agent, candidate string, force bool) error {
		if candidate == filename {
			once.Do(func() { close(entered); <-releaseAll })
		}
		return oldRetry(root, agent, candidate, force)
	}
	t.Cleanup(func() { retryDLQMessage = oldRetry })

	allDone := make(chan error, 1)
	go func() { allDone <- retryAllDLQ(deliveryRoot, "alice", false, false) }()
	<-entered
	if err := oldRetry(deliveryRoot, "alice", filename, false); err != nil {
		t.Fatalf("single retry winner: %v", err)
	}
	close(releaseAll)
	if err := <-allDone; err != nil {
		t.Fatalf("retry-all loser = %v, want idempotent terminal success", err)
	}
	env, _, err := fsq.ReadDLQEnvelopePath(filepath.Join(fsq.AgentDLQCur(root, "alice"), filename))
	if err != nil || env.RetryCount != 1 {
		t.Fatalf("audit after all/single race = %#v err=%v, want retry_count 1", env, err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "all-single-race.md")); err != nil {
		t.Fatalf("single consumer-visible copy missing: %v", err)
	}
}

func TestRunDLQRetryAllTreatsDeliveredAuditAsIdempotent(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "retry-all-terminal")
	filename := filepath.Base(dlqPath)
	dlqID := strings.TrimSuffix(filename, ".md")
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	if err := fsq.RetryFromDLQ(deliveryRoot, "alice", filename, false); err != nil {
		t.Fatalf("initial retry: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{"--root", root, "--me", "alice", "--all", "--json"})
	})
	if err != nil {
		t.Fatalf("idempotent retry-all rerun: %v", err)
	}
	var result struct {
		Retried          []string `json:"retried"`
		AlreadyDelivered []string `json:"already_delivered"`
		Skipped          []string `json:"skipped"`
		Count            int      `json:"count"`
	}
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("decode idempotent retry-all result: %v (output: %s)", err, stdout)
	}
	if result.Count != 0 || len(result.Retried) != 0 || len(result.Skipped) != 0 || !containsString(result.AlreadyDelivered, dlqID) {
		t.Fatalf("idempotent retry-all result = %#v, want only already-delivered %q", result, dlqID)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "retry-all-terminal.md")); statErr != nil {
		t.Fatalf("idempotent retry-all lost original delivery: %v", statErr)
	}

	text, _, err := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{"--root", root, "--me", "alice", "--all"})
	})
	if err != nil {
		t.Fatalf("idempotent retry-all text rerun: %v", err)
	}
	if !strings.Contains(text, "Already delivered: 1 message(s).") || strings.Contains(text, "No DLQ messages retried.") {
		t.Fatalf("idempotent retry-all text = %q, want delivered summary without no-op failure wording", text)
	}
}

func TestRunDLQRetryAllLeavesLegacyIndeterminateAuditSkipped(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const (
		dlqID    = "legacy-indeterminate-all"
		filename = dlqID + ".md"
	)
	raw := []byte("---\n" +
		`{"schema":"amq/dlq/v1","id":"legacy-indeterminate-all","original_id":"legacy-original","original_file":"legacy-original.md","failure_reason":"parse_error","failure_detail":"legacy fixture","failure_time":"2026-07-28T00:00:00Z","retry_count":1,"source_dir":"new"}` +
		"\n---\nlegacy body")
	path := filepath.Join(fsq.AgentDLQNew(root, "alice"), filename)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write legacy DLQ envelope: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{"--root", root, "--me", "alice", "--all", "--json"})
	})
	if !errors.Is(err, fsq.ErrDLQRetryIndeterminate) {
		t.Fatalf("legacy indeterminate retry-all = %v, want nonzero indeterminate error", err)
	}
	var result struct {
		Retried          []string `json:"retried"`
		AlreadyDelivered []string `json:"already_delivered"`
		Skipped          []string `json:"skipped"`
		Count            int      `json:"count"`
	}
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("decode legacy retry-all result: %v (output: %s)", err, stdout)
	}
	if result.Count != 0 || len(result.Retried) != 0 || len(result.AlreadyDelivered) != 0 || !containsString(result.Skipped, filename) {
		t.Fatalf("legacy indeterminate retry-all result = %#v, want only skipped %q", result, filename)
	}
	if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, raw) {
		t.Fatalf("legacy indeterminate retry-all mutated envelope: bytes=%q err=%v", after, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "legacy-original.md")); !os.IsNotExist(statErr) {
		t.Fatalf("legacy indeterminate retry-all recreated inbox/new delivery: %v", statErr)
	}
}

func TestRunDLQRetryAllOutputsSuccessesAndReturnsJoinedItemErrors(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")

	successPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "retry-success")
	blockedPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "retry-blocked")
	deliverInvalidDLQTransitionFixture(t, root, "alice", "retry-blocked")

	corruptPath := filepath.Join(fsq.AgentDLQNew(root, "alice"), "retry-corrupt.md")
	corruptBefore := []byte("corrupt retry-all envelope")
	if err := os.WriteFile(corruptPath, corruptBefore, 0o600); err != nil {
		t.Fatalf("write corrupt retry-all fixture: %v", err)
	}
	blockedBefore, err := os.ReadFile(blockedPath)
	if err != nil {
		t.Fatalf("read blocked envelope before retry-all: %v", err)
	}

	stdout, _, retryErr := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{
			"--root", root,
			"--me", "alice",
			"--all",
			"--json",
		})
	})

	var result struct {
		Retried []string `json:"retried"`
		Skipped []string `json:"skipped"`
		Count   int      `json:"count"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode retry-all result: %v (output: %s)", decodeErr, stdout)
	}
	successID := strings.TrimSuffix(filepath.Base(successPath), ".md")
	blockedFile := filepath.Base(blockedPath)
	corruptFile := filepath.Base(corruptPath)
	if result.Count != 1 || !containsString(result.Retried, successID) {
		t.Fatalf("retry-all successes = %#v count=%d, want %q", result.Retried, result.Count, successID)
	}
	for _, skipped := range []string{blockedFile, corruptFile} {
		if !containsString(result.Skipped, skipped) {
			t.Fatalf("retry-all skipped = %#v, want %q", result.Skipped, skipped)
		}
		if retryErr == nil || !strings.Contains(retryErr.Error(), skipped) {
			t.Fatalf("retry-all error = %v, want item %q", retryErr, skipped)
		}
	}
	if retryErr == nil || GetExitCode(retryErr) != ExitError {
		t.Fatalf("retry-all error = %T %v (exit %d), want joined non-zero item errors", retryErr, retryErr, GetExitCode(retryErr))
	}
	for _, want := range []string{"original file already exists in inbox/new", "read dlq envelope"} {
		if !strings.Contains(retryErr.Error(), want) {
			t.Fatalf("retry-all error = %q, want cause %q", retryErr, want)
		}
	}

	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "retry-success.md")); statErr != nil {
		t.Fatalf("successful retry original missing from inbox/new: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentDLQCur(root, "alice"), filepath.Base(successPath))); statErr != nil {
		t.Fatalf("successful retry envelope missing from dlq/cur: %v", statErr)
	}
	assertFileBytes(t, blockedPath, blockedBefore)
	assertFileBytes(t, corruptPath, corruptBefore)
}

func TestRunDLQRetryAllIncludesInspectedCurAndDeduplicatesPartialEnvelope(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	inspectedPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "retry-inspected")
	inspectedFile := filepath.Base(inspectedPath)
	inspectedID := strings.TrimSuffix(inspectedFile, ".md")
	if _, _, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{
			"--root", root,
			"--me", "alice",
			"--id", inspectedID,
			"--json",
		})
	}); err != nil {
		t.Fatalf("inspect DLQ fixture: %v", err)
	}

	duplicateNewPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "retry-duplicate")
	duplicateFile := filepath.Base(duplicateNewPath)
	duplicateID := strings.TrimSuffix(duplicateFile, ".md")
	duplicateEnv, _, err := fsq.ReadDLQEnvelopePath(duplicateNewPath)
	if err != nil {
		t.Fatalf("read duplicate DLQ fixture: %v", err)
	}
	duplicateEnv.RetryCount = 1
	duplicateEnv.RetryState = fsq.RetryStateDelivered
	duplicateEnv.RetryPending = false
	duplicateEnv.RetryDelivered = true
	duplicateHeader, err := json.Marshal(duplicateEnv)
	if err != nil {
		t.Fatalf("marshal authoritative duplicate cur envelope: %v", err)
	}
	duplicateBytes := append([]byte("---\n"), duplicateHeader...)
	duplicateBytes = append(duplicateBytes, []byte("\n---\nauthoritative cur body")...)
	duplicateCurPath := filepath.Join(fsq.AgentDLQCur(root, "alice"), duplicateFile)
	if err := os.WriteFile(duplicateCurPath, duplicateBytes, 0o600); err != nil {
		t.Fatalf("write duplicate cur envelope: %v", err)
	}

	stdout, _, retryErr := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{
			"--root", root,
			"--me", "alice",
			"--all",
			"--json",
		})
	})
	if retryErr != nil {
		t.Fatalf("retry all inspected/deduplicated fixtures: %v", retryErr)
	}
	var result struct {
		Retried          []string `json:"retried"`
		AlreadyDelivered []string `json:"already_delivered"`
		Skipped          []string `json:"skipped"`
		Count            int      `json:"count"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode retry-all result: %v (output: %s)", decodeErr, stdout)
	}
	if result.Count != 1 || len(result.Retried) != 1 || len(result.Skipped) != 0 || len(result.AlreadyDelivered) != 1 {
		t.Fatalf("retry-all result = %#v, want one retry plus one terminal idempotent result", result)
	}
	if countString(result.Retried, inspectedID) != 1 {
		t.Fatalf("retry-all retried = %#v, want %q exactly once", result.Retried, inspectedID)
	}
	if countString(result.AlreadyDelivered, duplicateID) != 1 {
		t.Fatalf("retry-all already_delivered = %#v, want %q exactly once", result.AlreadyDelivered, duplicateID)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "retry-inspected.md")); statErr != nil {
		t.Fatalf("retried inspected original missing from inbox/new: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "retry-duplicate.md")); !os.IsNotExist(statErr) {
		t.Fatalf("terminal duplicate recreated inbox/new delivery: %v", statErr)
	}
	for _, fixture := range []struct {
		filename string
		newPath  string
	}{
		{filename: inspectedFile, newPath: inspectedPath},
		{filename: duplicateFile, newPath: duplicateNewPath},
	} {
		if _, statErr := os.Stat(fixture.newPath); !os.IsNotExist(statErr) {
			t.Fatalf("retried envelope remains in dlq/new: %s: %v", fixture.newPath, statErr)
		}
		env, _, readErr := fsq.ReadDLQEnvelopePath(filepath.Join(fsq.AgentDLQCur(root, "alice"), fixture.filename))
		if readErr != nil || env.RetryCount != 1 {
			t.Fatalf("retried cur envelope %s retry_count=%v err=%v", fixture.filename, env, readErr)
		}
	}
}

func TestRunDLQRetryAllContinuesAfterOneDirectoryScanFails(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "retry-after-scan-error")
	dlqFile := filepath.Base(dlqPath)
	dlqID := strings.TrimSuffix(dlqFile, ".md")
	if _, _, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{
			"--root", root,
			"--me", "alice",
			"--id", dlqID,
			"--json",
		})
	}); err != nil {
		t.Fatalf("inspect DLQ fixture: %v", err)
	}

	injectedErr := errors.New("injected dlq/new scan failure")
	oldReadDir := readDLQRetryDir
	readDLQRetryDir = func(deliveryRoot *fsq.DeliveryRoot, dir string) ([]os.DirEntry, error) {
		if dir == filepath.Join("agents", "alice", "dlq", fsq.BoxNew) {
			return nil, injectedErr
		}
		return oldReadDir(deliveryRoot, dir)
	}
	t.Cleanup(func() { readDLQRetryDir = oldReadDir })

	stdout, _, retryErr := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{
			"--root", root,
			"--me", "alice",
			"--all",
			"--json",
		})
	})
	if !errors.Is(retryErr, injectedErr) {
		t.Fatalf("retry-all error = %v, want scan failure", retryErr)
	}
	var result struct {
		Retried []string `json:"retried"`
		Skipped []string `json:"skipped"`
		Count   int      `json:"count"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode retry-all scan-error result: %v (output: %s)", decodeErr, stdout)
	}
	if result.Count != 1 || !containsString(result.Retried, dlqID) || len(result.Skipped) != 0 {
		t.Fatalf("retry-all scan-error result = %#v, want cur success", result)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "retry-after-scan-error.md")); statErr != nil {
		t.Fatalf("retry-all did not continue into dlq/cur: %v", statErr)
	}
}

func TestRunDLQRetryOutputsCommittedInboxDeliveryBeforeError(t *testing.T) {
	for _, test := range []struct {
		name string
		all  bool
	}{
		{name: "single"},
		{name: "all", all: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := initializedSendMailboxRoot(t, "alice", "bob")
			const originalID = "retry-committed"
			dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", originalID)
			dlqFile := filepath.Base(dlqPath)
			dlqID := strings.TrimSuffix(dlqFile, ".md")
			injectedErr := installCommittedRetryAfterDelivery(t, dlqFile, originalID+".md")

			args := []string{"--root", root, "--me", "alice", "--id", dlqID, "--json"}
			if test.all {
				args = []string{"--root", root, "--me", "alice", "--all", "--json"}
			}
			stdout, _, retryErr := captureEnvOutput(t, func() error {
				return runDLQRetry(args)
			})
			var committed *fsq.CommittedDurabilityError
			if !errors.As(retryErr, &committed) || !errors.Is(retryErr, injectedErr) || GetExitCode(retryErr) != ExitError {
				t.Fatalf("retry error = %T %v (exit %d), want committed inbox delivery", retryErr, retryErr, GetExitCode(retryErr))
			}
			wantFinal := filepath.Join(fsq.AgentInboxNew(root, "alice"), originalID+".md")
			if committed.FinalPath != wantFinal || committed.Recipient != "alice" {
				t.Fatalf("committed retry metadata = %#v, want %q", committed, wantFinal)
			}
			if test.all {
				var result struct {
					Retried []string `json:"retried"`
					Skipped []string `json:"skipped"`
					Count   int      `json:"count"`
				}
				if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
					t.Fatalf("decode committed retry-all output: %v (output: %s)", decodeErr, stdout)
				}
				if result.Count != 1 || !containsString(result.Retried, dlqID) || len(result.Skipped) != 0 {
					t.Fatalf("committed retry-all output = %#v", result)
				}
			} else {
				var result struct {
					Retried string `json:"retried"`
				}
				if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
					t.Fatalf("decode committed single retry output: %v (output: %s)", decodeErr, stdout)
				}
				if result.Retried != dlqID {
					t.Fatalf("committed single retry output = %#v, want %q", result, dlqID)
				}
			}
			if _, statErr := os.Stat(wantFinal); statErr != nil {
				t.Fatalf("committed retried original missing: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(fsq.AgentDLQCur(root, "alice"), dlqFile)); statErr != nil {
				t.Fatalf("committed retry envelope missing from cur: %v", statErr)
			}
		})
	}
}

func TestRunDLQRetryAllKeepsCommittedEnvelopeUpdateInSkipped(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "retry-envelope-commit")
	dlqFile := filepath.Base(dlqPath)
	before, err := os.ReadFile(dlqPath)
	if err != nil {
		t.Fatalf("read envelope-update fixture: %v", err)
	}
	injectedErr := installCommittedRetryWithoutInboxDelivery(t, dlqFile)

	stdout, _, retryErr := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{"--root", root, "--me", "alice", "--all", "--json"})
	})
	var committed *fsq.CommittedDurabilityError
	if !errors.As(retryErr, &committed) || !errors.Is(retryErr, injectedErr) {
		t.Fatalf("retry-all error = %T %v, want committed envelope-update error", retryErr, retryErr)
	}
	var result struct {
		Retried []string `json:"retried"`
		Skipped []string `json:"skipped"`
		Count   int      `json:"count"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode envelope-commit output: %v (output: %s)", decodeErr, stdout)
	}
	if result.Count != 0 || len(result.Retried) != 0 || !containsString(result.Skipped, dlqFile) {
		t.Fatalf("envelope-update retry-all output = %#v", result)
	}
	assertFileBytes(t, dlqPath, before)
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "retry-envelope-commit.md")); !os.IsNotExist(statErr) {
		t.Fatalf("metadata-only commit redelivered original: %v", statErr)
	}
}

func TestDLQRetryAllSkipsDotfilesEvenWhenTheyParse(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	visible := writeOldValidDLQEnvelope(t, root, "alice", "visible-retry.md")
	data, err := os.ReadFile(visible)
	if err != nil {
		t.Fatalf("read visible envelope: %v", err)
	}
	hidden := filepath.Join(fsq.AgentDLQNew(root, "alice"), ".hidden-retry.md")
	if err := os.WriteFile(hidden, data, 0o600); err != nil {
		t.Fatalf("write hidden envelope: %v", err)
	}

	stdout, _, retryErr := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{"--root", root, "--me", "alice", "--all", "--json"})
	})
	if retryErr != nil {
		t.Fatalf("retry-all: %v (output: %s)", retryErr, stdout)
	}
	var result struct {
		Retried []string `json:"retried"`
		Skipped []string `json:"skipped"`
		Count   int      `json:"count"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode retry-all: %v (output: %s)", decodeErr, stdout)
	}
	if result.Count != 1 || !containsString(result.Retried, "visible-retry") {
		t.Fatalf("retry-all result = %#v, want visible envelope retried once", result)
	}
	if containsString(result.Retried, ".hidden-retry") || containsString(result.Skipped, ".hidden-retry.md") {
		t.Fatalf("hidden envelope was scanned: %#v", result)
	}
	if _, err := os.Stat(visible); !os.IsNotExist(err) {
		t.Fatalf("visible DLQ envelope was not moved from new: %v", err)
	}
	if _, err := os.Stat(hidden); err != nil {
		t.Fatalf("hidden DLQ envelope was retried away: %v", err)
	}
}

func moveInvalidFixtureToDLQForRetryAll(t *testing.T, root, agent, id string) string {
	t.Helper()
	deliverInvalidDLQTransitionFixture(t, root, agent, id)
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatalf("snapshot retry-all delivery root: %v", err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatalf("open retry-all delivery root: %v", err)
	}
	defer func() { _ = deliveryRoot.Close() }()
	path, err := fsq.MoveToDLQ(deliveryRoot, agent, id+".md", id, "parse_error", "retry-all fixture")
	if err != nil {
		t.Fatalf("move retry-all fixture to DLQ: %v", err)
	}
	return path
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func installCommittedRetryAfterDelivery(t *testing.T, targetFilename, originalFilename string) error {
	t.Helper()
	injectedErr := errors.New("injected committed retry inbox sync failure")
	oldRetry := retryDLQMessage
	retryDLQMessage = func(root *fsq.DeliveryRoot, agent, filename string, force bool) error {
		err := oldRetry(root, agent, filename, force)
		if err != nil || filename != targetFilename {
			return err
		}
		return &fsq.CommittedDurabilityError{
			FinalPath: root.DisplayPath(filepath.Join("agents", agent, "inbox", "new", originalFilename)),
			Recipient: agent,
			Err:       injectedErr,
		}
	}
	t.Cleanup(func() { retryDLQMessage = oldRetry })
	return injectedErr
}

func installCommittedRetryWithoutInboxDelivery(t *testing.T, targetFilename string) error {
	t.Helper()
	injectedErr := errors.New("injected committed retry envelope update")
	oldRetry := retryDLQMessage
	retryDLQMessage = func(root *fsq.DeliveryRoot, agent, filename string, force bool) error {
		if filename != targetFilename {
			return oldRetry(root, agent, filename, force)
		}
		return &fsq.CommittedDurabilityError{
			FinalPath: root.DisplayPath(filepath.Join("agents", agent, "dlq", "cur", filename)),
			Err:       injectedErr,
		}
	}
	t.Cleanup(func() { retryDLQMessage = oldRetry })
	return injectedErr
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read retained file %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("retained file %s changed", path)
	}
}
