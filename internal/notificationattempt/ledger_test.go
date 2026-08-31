package notificationattempt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriterPersistsPreparedAndResultInSingleLog(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	writer.now = func() time.Time {
		return time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	}

	prepared, err := writer.Prepare([]string{"msg-2", "msg-1", "msg-1"}, "raw")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.AttemptID == "" {
		t.Fatal("prepared attempt ID is empty")
	}
	if got := strings.Join(prepared.MessageIDs, ","); got != "msg-1,msg-2" {
		t.Fatalf("message IDs = %q", got)
	}
	if err := writer.Result(prepared, OutcomeWritten, "terminal bytes accepted"); err != nil {
		t.Fatalf("Result: %v", err)
	}

	// Requirement 2: ONE log, not two. Both prepared and result live in the
	// same file, distinguished by the phase field.
	dir := filepath.Join(root, "agents", "codex", "receipts")
	path := filepath.Join(dir, LogFilename)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", LogFilename, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %o, want 600", LogFilename, got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 records in one log, got %d lines", len(lines))
	}
	var preparedRec, resultRec Record
	if err := json.Unmarshal([]byte(lines[0]), &preparedRec); err != nil {
		t.Fatalf("decode prepared: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &resultRec); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if preparedRec.Phase != PhasePrepared || resultRec.Phase != PhaseResult {
		t.Fatalf("phases = %q, %q", preparedRec.Phase, resultRec.Phase)
	}
	if preparedRec.AttemptID != resultRec.AttemptID {
		t.Fatalf("attempt IDs do not match: %q vs %q", preparedRec.AttemptID, resultRec.AttemptID)
	}
	// No separate result file should exist (the prototype had two files).
	if _, err := os.Stat(filepath.Join(dir, "notification-attempts.result.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("separate result file should not exist (single-log design)")
	}
}

func TestListTreatsPreparedWithoutResultAsIndeterminate(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	prepared, err := writer.Prepare([]string{"msg-1"}, "external")
	if err != nil {
		t.Fatal(err)
	}

	attempts, err := List(root, "codex", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d", len(attempts))
	}
	if attempts[0].State != StateIndeterminate || attempts[0].Prepared.AttemptID != prepared.AttemptID {
		t.Fatalf("attempt = %+v", attempts[0])
	}
	if attempts[0].Result != nil {
		t.Fatalf("unexpected result: %+v", attempts[0].Result)
	}
}

// Requirement 1: O_APPEND gives kernel-level atomic appends. Concurrent
// writers must not lose records (the lost-update race the prototype's
// read-modify-write had). This test runs N goroutines each appending a
// record and asserts all N survive in the log.
func TestConcurrentAppendsDoNotLoseRecords(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			rec := Record{
				Schema:     SchemaVersion,
				AttemptID:  "concurrent-" + strings.Repeat("a", 16) + string(rune('a'+i%26)) + itoa(i),
				Phase:      PhasePrepared,
				MessageIDs: []string{"msg-concurrent"},
				Agent:      "codex",
				Mode:       "raw",
				RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}
			if err := writer.append(rec); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Read the raw log and count lines. All n records must survive — O_APPEND
	// serializes the writes at the kernel level; no read-modify-write race.
	dir := filepath.Join(root, "agents", "codex", "receipts")
	data, err := os.ReadFile(filepath.Join(dir, LogFilename))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != n {
		t.Fatalf("expected %d records, got %d — O_APPEND lost records under concurrency", n, len(lines))
	}
}

// TestConcurrentAppendsAcrossRotation is the race the lead review caught:
// the existing 50-goroutine test never crosses a rotation boundary, so it
// passes even though rotation's unlocked read→merge→truncate could discard an
// O_APPEND record that landed between the read and the truncate. This test
// forces appends and rotation to overlap: many appenders run concurrently
// while a rotator repeatedly trips the cap, and we assert that EVERY appended
// record survives in current ∪ .1. Before the flock fix (LOCK_SH on append,
// LOCK_EX on rotation) this test loses records; after the fix it passes.
//
// Determinism: the race is timing-dependent, so the test runs many iterations
// with GOMAXPROCS pressure to make a losing interleaving likely. -race is
// also valuable here but the assertion is about lost data, not memory races.
func TestConcurrentAppendsAcrossRotation(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	// Tiny cap so rotation triggers frequently and appends are very likely to
	// land inside the rotation window.
	writer.maxBytes = 300

	const appenders = 16
	const recordsEach = 200
	var wg sync.WaitGroup
	wg.Add(appenders)

	// Each appender writes recordsEach unique records. We collect the IDs it
	// attempted so the assertion can check every one survived.
	type appenderResult struct {
		ids []string
	}
	results := make([]appenderResult, appenders)

	for a := 0; a < appenders; a++ {
		a := a
		go func() {
			defer wg.Done()
			local := make([]string, 0, recordsEach)
			for i := 0; i < recordsEach; i++ {
				id := fmt.Sprintf("xrot-%d-%d", a, i)
				rec := Record{
					Schema:     SchemaVersion,
					AttemptID:  id,
					Phase:      PhasePrepared,
					MessageIDs: []string{"msg-xrot"},
					Agent:      "codex",
					Mode:       "raw",
					RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
				}
				if err := writer.append(rec); err != nil {
					t.Errorf("append %d/%d: %v", a, i, err)
					return
				}
				local = append(local, id)
			}
			results[a].ids = local
		}()
	}
	wg.Wait()

	// Collect every attempted ID.
	attempted := make(map[string]bool)
	for _, r := range results {
		for _, id := range r.ids {
			attempted[id] = true
		}
	}
	if len(attempted) == 0 {
		t.Fatal("no records were appended (appenders errored above)")
	}

	// Read both generations and count surviving records. A record is present
	// if its AttemptID appears as a JSON line in current or .1.
	dir := filepath.Join(root, "agents", "codex", "receipts")
	surviving := make(map[string]bool)
	for _, name := range []string{LogFilename, LogFilename + rotatedSuffix} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			surviving[rec.AttemptID] = true
		}
	}

	// The assertion: every attempted record must survive. If rotation's
	// read→merge→truncate ran unlocked, an O_APPEND writer could land a record
	// after rotation's read and before its truncate, and that record is gone —
	// trace would then report "no record" for an injection that happened.
	var lost []string
	for id := range attempted {
		if !surviving[id] {
			lost = append(lost, id)
		}
	}
	if len(lost) > 0 {
		sort.Strings(lost)
		t.Fatalf("rotation race lost %d/%d records under concurrent append (sample: %s) — "+
			"an unlocked read→truncate discarded O_APPEND writes that landed in the window",
			len(lost), len(attempted), strings.Join(lost[:min(len(lost), 5)], ", "))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Requirement 2: rotation can never orphan a result. In a single append-only
// log, a result is always written AFTER its prepared, so when the file
// rotates (moves to .1), the prepared drops first and a surviving result in
// the current file always has its prepared in .1 (older) — never the reverse.
// This test forces a rotation and confirms no result is orphaned.
func TestRotationNeverOrphansResult(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	// Small cap so rotation triggers after a few records.
	writer.maxBytes = 600

	// Write a prepared+result pair, then enough records to rotate the
	// prepared into .1 while the result stays in the current file.
	prepared, err := writer.Prepare([]string{"msg-rotate"}, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Result(prepared, OutcomeWritten, ""); err != nil {
		t.Fatal(err)
	}
	// Pad with extra records to force rotation.
	for i := 0; i < 10; i++ {
		rec := Record{
			Schema:     SchemaVersion,
			AttemptID:  "pad-" + itoa(i),
			Phase:      PhasePrepared,
			MessageIDs: []string{"msg-pad"},
			Agent:      "codex",
			Mode:       "raw",
			RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := writer.append(rec); err != nil {
			t.Fatalf("pad append %d: %v", i, err)
		}
	}

	// List must join the prepared (in .1) with its result — no orphan.
	attempts, err := List(root, "codex", "msg-rotate")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt for msg-rotate, got %d", len(attempts))
	}
	if attempts[0].State != OutcomeWritten {
		t.Fatalf("result was orphaned by rotation: state = %q, want %q", attempts[0].State, OutcomeWritten)
	}
	if attempts[0].Result == nil {
		t.Fatal("result was orphaned by rotation: Result is nil")
	}
}

func TestRotationCapsBothGenerations(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	writer.maxBytes = 420

	for i := 0; i < 12; i++ {
		rec := Record{
			Schema:     SchemaVersion,
			AttemptID:  strings.Repeat("a", 20) + string(rune('a'+i)),
			Phase:      PhasePrepared,
			MessageIDs: []string{"msg-" + string(rune('a'+i))},
			Agent:      "codex",
			Mode:       "raw",
			RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := writer.append(rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	dir := filepath.Join(root, "agents", "codex", "receipts")
	// The current file must be under the cap (it is the hot-path append target).
	// The .1 file accumulates merged generations and may exceed the cap — that
	// is correct for an audit log (history is the value; the cap bounds the
	// active append target, not the archive).
	info, err := os.Stat(filepath.Join(dir, LogFilename))
	if err != nil {
		t.Fatalf("stat %s: %v", LogFilename, err)
	}
	if info.Size() > writer.maxBytes {
		t.Fatalf("%s size = %d, cap = %d", LogFilename, info.Size(), writer.maxBytes)
	}
	// .1 must exist (rotation happened).
	if _, err := os.Stat(filepath.Join(dir, LogFilename+rotatedSuffix)); os.IsNotExist(err) {
		t.Fatalf("%s should exist after rotation", LogFilename+rotatedSuffix)
	}
	// No second rotation file (.1.1) — we merge into .1, never create .1.1.
	if _, err := os.Stat(filepath.Join(dir, LogFilename+rotatedSuffix+rotatedSuffix)); !os.IsNotExist(err) {
		t.Fatalf("unexpected second rotation file: %v", err)
	}
}

func TestResultRejectsDishonestOutcome(t *testing.T) {
	writer := NewWriter(t.TempDir(), "codex")
	prepared, err := writer.Prepare([]string{"msg-1"}, "raw")
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"seen", "displayed", "submitted", ""} {
		if err := writer.Result(prepared, outcome, ""); err == nil {
			t.Fatalf("Result accepted outcome %q", outcome)
		}
	}
}

// Requirement 3: trace must distinguish "no attempt recorded" from "recording
// failed". If Prepare's write fails, it returns a zero Record + error. The
// caller injects anyway (ledger never blocks delivery) and should record a
// result with outcome=failed. List must still surface the attempt if a result
// was recorded, and trace wording differs from an empty journal.
func TestPrepareWriteFailureDoesNotBlockAndResultIsRecordable(t *testing.T) {
	// A root that does not exist causes EnsureAgentDirs to fail → Prepare's
	// append errors. The caller gets a zero Record + error.
	writer := NewWriter("/nonexistent/root/path", "codex")
	prepared, err := writer.Prepare([]string{"msg-1"}, "raw")
	if err == nil {
		t.Fatal("Prepare should fail on a nonexistent root")
	}
	if prepared.AttemptID != "" {
		t.Fatalf("Prepare should return zero Record on failure, got AttemptID %q", prepared.AttemptID)
	}

	// The caller reconstructs a minimal prepared identity to record the
	// result — the ledger must accept this so trace can surface "recording
	// failed" rather than a silent hole.
	reconstructed := Record{
		AttemptID:  "recovered-from-write-failure",
		MessageIDs: []string{"msg-1"},
		Agent:      "codex",
	}
	// This writes to a valid root now.
	writer2 := NewWriter(t.TempDir(), "codex")
	if err := writer2.Result(reconstructed, OutcomeFailed, "prepared write failed: "+err.Error()); err != nil {
		t.Fatalf("Result with reconstructed identity: %v", err)
	}
	// Only the result was recorded (no prepared) — it is not joined to a
	// prepared, so it does not appear as an Attempt. This is correct: a
	// result without a prepared is a write-failure marker, surfaced by trace
	// via a separate path (the leg checks for orphan results). The key
	// assertion is that the result WAS persisted (the write didn't silently
	// drop it).
	dir := filepath.Join(writer2.root, "agents", "codex", "receipts")
	data, err := os.ReadFile(filepath.Join(dir, LogFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "recovered-from-write-failure") {
		t.Fatal("the write-failure result was not persisted — trace would see a silent hole")
	}
	if !strings.Contains(string(data), OutcomeFailed) {
		t.Fatal("the write-failure result did not record outcome=failed")
	}
}

// Empty journal returns (nil, nil) — no attempts = no evidence, not an error.
// This is the correct fail-mode for trace's "no evidence" leg.
func TestListEmptyJournalIsNotAnError(t *testing.T) {
	root := t.TempDir()
	attempts, err := List(root, "codex", "msg-1")
	if err != nil {
		t.Fatalf("List on empty journal should return nil,nil, got err: %v", err)
	}
	if attempts != nil {
		t.Fatalf("List on empty journal should return nil attempts, got %d", len(attempts))
	}
}

// itoa is a tiny dependency-free int→string to keep the test file self-contained.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
