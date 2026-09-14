package requests

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestRecordBudgetFitsWorstCaseEncoding reproduces Pro finding B5 on
// agent-message-queue-611.22.35 (Pro merged-stack review): MaxResultBytes and
// MaxInputBytes bound RAW text, but the store enforces MaxRecordBytes on the
// JSON-ENCODED record. With MaxRecordBytes = result + input + overhead, a
// 512 KiB result of ordinary newline-heavy text encoded to 1.5x and was
// refused as storage_full on a healthy disk; quote-heavy text hit 2x and
// control-heavy text 6x. The invariant is that ANY result within
// MaxResultBytes fits the record, for ANY content, so the budget is derived
// from encoding/json's worst-case expansion. This walks a record through the
// store with worst-case text — every byte a control character that escapes to
// a six-byte \u00XX sequence — at both bounds, and asserts the store accepts
// it at every revision.
func TestRecordBudgetFitsWorstCaseEncoding(t *testing.T) {
	worstResult := strings.Repeat("\x01", protocol.MaxResultBytes)
	worstInput := strings.Repeat("\x01", protocol.MaxInputBytes)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	rec := &Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   "11111111-1111-4111-8111-1111111111b5",
			CreatorHost: "amq:codex.proj.deadbeef",
			TargetID:    "t_worstcase",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			NotAfter:    "2026-09-08T10:02:00Z",
		},
		Input: &protocol.SubmitInput{Text: worstInput, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn},
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("create with worst-case input: %v", err)
	}

	rec.Revision = 2
	rec.State = protocol.StateDispatching
	if err := store.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}

	rec.Revision = 3
	rec.State = protocol.StateCompleted
	rec.Result = &protocol.Result{Text: worstResult, Truncated: true}
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(encoded) > protocol.MaxRecordBytes {
		t.Fatalf("worst-case record encodes to %d bytes, budget is %d: a valid bounded result does not fit its own record", len(encoded), protocol.MaxRecordBytes)
	}
	if err := store.Update(rec); err != nil {
		t.Fatalf("store refused a worst-case-encoded but in-bounds terminal record: %v", err)
	}
}
