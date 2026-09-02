package notificationattempt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rotation failure around a SUCCESSFUL append must surface as a typed
// RotationError, not a plain error — callers that treat any append error as
// "record lost" fabricate a failure result that joins the real prepared
// record into a false `failed` attempt. Rotation is forced to fail by making
// the .1 archive path un-writable (a directory in its place).
func TestAppendRotationFailureIsTypedAndRecordPersists(t *testing.T) {
	root := t.TempDir()
	writer := NewWriterWithMaxBytes(root, "codex", 400)

	// Fill past the cap so the next append rotates.
	if _, err := writer.Prepare([]string{"msg-fill-1"}, "raw"); err != nil {
		t.Fatalf("fill 1: %v", err)
	}
	if _, err := writer.Prepare([]string{"msg-fill-2"}, "raw"); err != nil && !IsRotationOnly(err) {
		t.Fatalf("fill 2: %v", err)
	}

	// Sabotage the archive path: a DIRECTORY named notification-attempts.jsonl.1
	// makes WriteFileAtomic's rename fail.
	dir := filepath.Join(root, "agents", "codex", "receipts")
	rotated := filepath.Join(dir, LogFilename+RotatedSuffix)
	_ = os.Remove(rotated)
	if err := os.Mkdir(rotated, 0o700); err != nil {
		t.Fatal(err)
	}

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
