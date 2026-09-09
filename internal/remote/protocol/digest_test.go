package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCommandDigestIsCanonicalAndStable pins the B13 digest contract: the
// digest covers the immutable submit command payload (schema, op, request_id,
// target_id, epoch, not_after, input), is stable across equal commands, and
// changes when epoch, not_after, or input changes. It must be sha256-prefixed
// hex and computed over canonical JSON (no insignificant whitespace).
func TestCommandDigestIsCanonicalAndStable(t *testing.T) {
	base := &Command{
		Schema:    SchemaCommand,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111501",
		TargetID:  "t_fake1",
		Epoch:     "e_1",
		NotAfter:  "2026-09-08T10:02:00Z",
		Input:     &SubmitInput{Text: "say hi"},
	}
	d := CommandDigest(base)
	if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
		t.Fatalf("digest %q is not sha256 hex", d)
	}
	// Stable across equal commands.
	if got := CommandDigest(base); got != d {
		t.Fatalf("digest not stable: %q vs %q", got, d)
	}
	// Changing the input text changes the digest.
	changed := *base
	changed.Input = &SubmitInput{Text: "say bye"}
	if CommandDigest(&changed) == d {
		t.Fatal("digest unchanged after input text change")
	}
	// Changing the epoch changes the digest (the B13 fix: a retry with a
	// changed epoch is request_conflict, not a silent dedup).
	changed = *base
	changed.Epoch = "e_2"
	if CommandDigest(&changed) == d {
		t.Fatal("digest unchanged after epoch change")
	}
	// Changing not_after changes the digest.
	changed = *base
	changed.NotAfter = "2026-09-08T11:00:00Z"
	if CommandDigest(&changed) == d {
		t.Fatal("digest unchanged after not_after change")
	}
	// Changing target_id changes the digest.
	changed = *base
	changed.TargetID = "t_other"
	if CommandDigest(&changed) == d {
		t.Fatal("digest unchanged after target_id change")
	}
	// Non-submit ops have no digest.
	nonSubmit := *base
	nonSubmit.Op = OpRequestGet
	nonSubmit.RequestRef = "amqr1_" + strings.Repeat("a", 20)
	if CommandDigest(&nonSubmit) != "" {
		t.Fatalf("non-submit op returned a digest: %q", CommandDigest(&nonSubmit))
	}
}

// TestCommandDigestCanonicalBytes pins the exact canonical JSON shape so the
// io lane (endpoint) and any other carrier that builds the bytes themselves
// agree with CommandDigest. The canonical payload is a JSON object with keys
// in struct order and no insignificant whitespace.
func TestCommandDigestCanonicalBytes(t *testing.T) {
	cmd := &Command{
		Schema:    SchemaCommand,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111501",
		TargetID:  "t_fake1",
		Epoch:     "e_1",
		NotAfter:  "2026-09-08T10:02:00Z",
		Input:     &SubmitInput{Text: "say hi", Busy: BusyQueue, Deliver: DeliverTurn},
	}
	// The canonical payload must marshal with keys in the fixed struct order
	// and no extra whitespace, so re-marshalling is byte-identical.
	payload := digestPayload{
		Schema: cmd.Schema, Op: cmd.Op, RequestID: cmd.RequestID,
		TargetID: cmd.TargetID, Epoch: cmd.Epoch, NotAfter: cmd.NotAfter,
		Input: cmd.Input,
	}
	b1, _ := json.Marshal(payload)
	b2, _ := json.Marshal(payload)
	if string(b1) != string(b2) {
		t.Fatal("canonical payload marshal not deterministic")
	}
	// Keys appear in the canonical order.
	wantOrder := []string{`"schema"`, `"op"`, `"request_id"`, `"target_id"`, `"epoch"`, `"not_after"`, `"input"`}
	pos := 0
	for i, k := range wantOrder {
		idx := strings.Index(string(b1), k)
		if idx < pos {
			t.Fatalf("key %s out of canonical order at step %d (idx=%d pos=%d) in %s", k, i, idx, pos, b1)
		}
		pos = idx + len(k)
	}
}

// TestReplyShape pins the B11 reply-envelope JSON shape: the snapshot is the
// immutable record revision and the outcome carries the op-specific code, both
// under their own keys.
func TestReplyShape(t *testing.T) {
	rep := Reply{
		Snapshot: Snapshot{
			Schema: SchemaRequest, RequestRef: "amqr1_" + strings.Repeat("a", 20),
			RequestID: "11111111-1111-4111-8111-111111111501",
			CreatorHost: "hostA", TargetID: "t_fake1", Epoch: "e_1",
			Revision: 1, State: StateReceived,
			ObservedAt: "2026-09-08T10:00:00Z",
		},
		Outcome: Outcome{Op: OpRequestSubmit, Code: CodeRequestConflict, Message: "input digest mismatch"},
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, key := range []string{`"snapshot"`, `"outcome"`, `"op"`, `"code"`, `"message"`} {
		if !strings.Contains(s, key) {
			t.Fatalf("reply JSON missing %s: %s", key, s)
		}
	}
	// A plain-success outcome omits code/message (omitempty).
	rep.Outcome = Outcome{Op: OpRequestSubmit}
	b, _ = json.Marshal(rep)
	s = string(b)
	if strings.Contains(s, `"code"`) || strings.Contains(s, `"message"`) {
		t.Fatalf("success outcome should omit code/message: %s", s)
	}
}
