package notificationattempt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A rotation failure around a SUCCESSFUL append must surface as a typed
// RotationError, not a plain error — callers that treat any append error as
// "record lost" fabricate a failure result that joins the real prepared
// record into a false `failed` attempt. Rotation is forced to fail by making
// the .1 archive path un-writable (a directory in its place).
func TestAppendRotationFailureIsTypedAndRecordPersists(t *testing.T) {
	root := t.TempDir()
	writer := NewWriterWithMaxBytes(root, "codex", 400)
	sabotageRotation(t, root, writer)
	dir := filepath.Join(root, "agents", "codex", "receipts")

	prepared, err := writer.Prepare([]string{"msg-under-rotation-failure"}, "raw")
	if err == nil {
		t.Fatal("expected a rotation error")
	}
	if !IsRotationOnly(err) {
		t.Fatalf("rotation failure not typed RotationError: %v", err)
	}
	if prepared.AttemptID == "" {
		t.Fatal("rotation-only error must still return the persisted record")
	}
	// The record must actually be on disk despite the rotation failure.
	raw, readErr := os.ReadFile(filepath.Join(dir, LogFilename))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(raw), "msg-under-rotation-failure") {
		t.Fatal("record was lost despite rotation-only error contract")
	}
	// And a genuinely lost append (unwritable journal) must NOT be typed.
	writer2 := NewWriter("/nonexistent/root/path", "codex")
	if _, err := writer2.Prepare([]string{"msg-lost"}, "raw"); err == nil || IsRotationOnly(err) {
		t.Fatalf("lost append misclassified as rotation-only: %v", err)
	}
}

// An invalid fold (contradictory terminal lifecycle + legacy result) must keep
// the contradicting legacy record in History — trace shows evidence, not just
// the verdict.
func TestInvalidFoldKeepsContradictingLegacyRecordInHistory(t *testing.T) {
	writer := NewWriter(t.TempDir(), "codex")
	lifecycle, err := writer.Begin([]string{"msg-evidence"}, "inject-via")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := writer.Transition(lifecycle, StateAccepted, ""); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	prepared := Record{
		Schema: SchemaVersion, AttemptID: lifecycle.AttemptID, Phase: PhasePrepared,
		MessageIDs: lifecycle.MessageIDs, Agent: lifecycle.Agent, Mode: lifecycle.Mode, State: StateAttempt,
	}
	if err := writer.Result(prepared, OutcomeWritten, "contradicts terminal accepted"); err != nil {
		t.Fatalf("Result: %v", err)
	}
	attempts, err := List(writer.root, "codex", "msg-evidence")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(attempts) != 1 || attempts[0].State != StateInvalid {
		t.Fatalf("attempts = %+v, want one invalid", attempts)
	}
	found := false
	for _, rec := range attempts[0].History {
		if rec.Outcome == OutcomeWritten {
			found = true
		}
	}
	if !found {
		t.Fatal("invalid fold dropped the contradicting legacy record from History")
	}
}

// sabotageRotation fills the journal past the writer's cap and puts a
// DIRECTORY named notification-attempts.jsonl.1 at the archive path, so
// WriteFileAtomic's rename fails and every later append persists its record
// but fails rotation (typed RotationError).
func sabotageRotation(t *testing.T, root string, writer *Writer) (rotatedPath string) {
	t.Helper()
	if _, err := writer.Prepare([]string{"msg-fill-1"}, "raw"); err != nil {
		t.Fatalf("fill 1: %v", err)
	}
	if _, err := writer.Prepare([]string{"msg-fill-2"}, "raw"); err != nil && !IsRotationOnly(err) {
		t.Fatalf("fill 2: %v", err)
	}
	rotated := filepath.Join(root, "agents", writer.agent, "receipts", LogFilename+RotatedSuffix)
	_ = os.Remove(rotated)
	if err := os.Mkdir(rotated, 0o700); err != nil {
		t.Fatal(err)
	}
	return rotated
}

// A rotation-only failure around a Transition must still advance the caller's
// lifecycle handle: the record landed. An implementation that returns before
// advancing leaves State/Sequence stale, the next Transition re-writes the
// same sequence, and foldLifecycle rejects the attempt as invalid — a
// delivered notification then traces as invalid because the journal was over
// its size cap.
func TestTransitionRotationFailureAdvancesLifecycle(t *testing.T) {
	root := t.TempDir()
	writer := NewWriterWithMaxBytes(root, "codex", 400)
	rotated := sabotageRotation(t, root, writer)

	lifecycle, err := writer.Begin([]string{"msg-transition-rotation"}, "inject-via")
	if !IsRotationOnly(err) || lifecycle == nil {
		t.Fatalf("Begin under rotation failure = (%v, %v), want lifecycle + rotation-only error", lifecycle, err)
	}
	err = writer.Transition(lifecycle, StateDeferred, "provider busy")
	if !IsRotationOnly(err) {
		t.Fatalf("Transition under rotation failure = %v, want rotation-only error", err)
	}
	if lifecycle.State != StateDeferred || lifecycle.Sequence != 1 {
		t.Fatalf("lifecycle after rotation-only transition = %s/%d, want deferred/1", lifecycle.State, lifecycle.Sequence)
	}
	if err := writer.Transition(lifecycle, StateRetried, "provider retry attempted"); !IsRotationOnly(err) {
		t.Fatalf("second Transition = %v, want rotation-only error", err)
	}
	if lifecycle.Sequence != 2 {
		t.Fatalf("lifecycle sequence after second transition = %d, want 2", lifecycle.Sequence)
	}
	// Every record lives in the current journal; clear the sabotage so List
	// can read the receipts directory.
	if err := os.Remove(rotated); err != nil {
		t.Fatal(err)
	}
	attempts, err := List(root, "codex", "msg-transition-rotation")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(attempts) != 1 || attempts[0].State != StateRetried || len(attempts[0].History) != 3 {
		t.Fatalf("attempts = %+v, want one retried attempt with a 3-record history", attempts)
	}
}

// WriteFailure reconstructs the identity of an attempt whose prepared write
// was lost. List must surface it as write_failed carrying the mode and the
// same normalized ids a persisted prepared record would have had; an
// implementation that copies the raw ids or drops Mode produces an orphan that
// does not join a prepared record's shape (Mode:"" and unsorted duplicates).
func TestWriteFailureSurfacesModeAndNormalizedIDs(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	if err := writer.WriteFailure("attempt-lost", []string{" msg-b ", "msg-a", "msg-b"}, "external", os.ErrPermission); err != nil {
		t.Fatalf("WriteFailure: %v", err)
	}
	attempts, err := List(root, "codex", "msg-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(attempts) != 1 || attempts[0].State != StateWriteFailed {
		t.Fatalf("attempts = %+v, want one write_failed attempt", attempts)
	}
	got := attempts[0]
	if got.Prepared.Mode != "external" || got.Result == nil || got.Result.Mode != "external" {
		t.Fatalf("write-failed attempt lost its mode: prepared %q result %+v", got.Prepared.Mode, got.Result)
	}
	if !sameStrings(got.Prepared.MessageIDs, []string{"msg-a", "msg-b"}) || !sameStrings(got.Result.MessageIDs, []string{"msg-a", "msg-b"}) {
		t.Fatalf("write-failed ids not normalized: prepared %v result %v", got.Prepared.MessageIDs, got.Result.MessageIDs)
	}
	if !strings.Contains(got.Result.Detail, "prepared write failed") || !strings.Contains(got.Result.Detail, os.ErrPermission.Error()) {
		t.Fatalf("write-failed detail = %q, want the write error", got.Result.Detail)
	}
	if err := writer.WriteFailure("attempt-empty", []string{" ", ""}, "external", nil); err == nil {
		t.Fatal("WriteFailure with no usable ids must refuse, not persist an unjoinable record")
	}
}

// A v2 prepared record whose only companion is a legacy result with a
// different identity (message set) is contradictory: the fold must stay
// invalid AND keep the mismatched result in History as evidence. Dropping it
// shows `invalid` with nothing to explain why.
func TestLegacyResultWithMismatchedIdentityStaysInvalidWithEvidence(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	lifecycle, err := writer.Begin([]string{"msg-mismatch"}, "external")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	mismatched := Record{
		Schema:     SchemaVersion,
		AttemptID:  lifecycle.AttemptID,
		Phase:      PhaseResult,
		MessageIDs: []string{"msg-mismatch", "msg-other"},
		Agent:      "codex",
		Mode:       "external",
		RecordedAt: writer.now().UTC().Format(time.RFC3339Nano),
		Outcome:    OutcomeWritten,
		Detail:     "identity drift",
	}
	if err := writer.append(mismatched); err != nil {
		t.Fatalf("append mismatched legacy result: %v", err)
	}
	attempts, err := List(root, "codex", "msg-mismatch")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(attempts) != 1 || attempts[0].State != StateInvalid {
		t.Fatalf("attempts = %+v, want one invalid attempt", attempts)
	}
	found := false
	for _, rec := range attempts[0].History {
		if rec.Detail == "identity drift" {
			found = true
		}
	}
	if !found {
		t.Fatal("mismatched legacy result dropped from History; invalid verdict has no evidence")
	}
}
