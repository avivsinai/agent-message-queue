package protocol

import (
	"crypto/sha256"
	"encoding/hex"
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
	// This test pins the CROSS-CARRIER contract, so it must exercise
	// CommandDigest itself. The previous version marshalled digestPayload
	// twice and compared the results: it never called CommandDigest, so
	// swapping the implementation to sorted keys — the thing the doc
	// wrongly promised — would have left it green.
	got := CommandDigest(cmd)

	// A carrier in another language builds these bytes by hand, in exactly
	// this key order, and must arrive at the same digest.
	canonical := `{"schema":"` + string(SchemaCommand) + `","op":"` + string(OpRequestSubmit) +
		`","request_id":"11111111-1111-4111-8111-111111111501","target_id":"t_fake1","epoch":"e_1",` +
		`"not_after":"2026-09-08T10:02:00Z","input":{"text":"say hi","busy":"queue","deliver":"turn"}}`
	sum := sha256.Sum256([]byte(canonical))
	want := digestPrefix + hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("CommandDigest = %s\nwant       = %s\n(canonical bytes: %s)", got, want, canonical)
	}

	// Sorting the keys — what the doc used to claim — must NOT agree, so a
	// future drift back to that wording is caught here rather than in the
	// field as request_conflict on every retry.
	sorted := `{"epoch":"e_1","input":{"busy":"queue","deliver":"turn","text":"say hi"},` +
		`"not_after":"2026-09-08T10:02:00Z","op":"` + string(OpRequestSubmit) +
		`","request_id":"11111111-1111-4111-8111-111111111501","schema":"` + string(SchemaCommand) +
		`","target_id":"t_fake1"}`
	sortedSum := sha256.Sum256([]byte(sorted))
	if got == digestPrefix+hex.EncodeToString(sortedSum[:]) {
		t.Fatal("digest matches the sorted-key form; the canonical order is the struct order, not alphabetical")
	}
}

// TestReplyShape pins the B11 reply-envelope JSON shape: the snapshot is the
// immutable record revision and the outcome carries the op-specific code, both
// under their own keys.
func TestReplyShape(t *testing.T) {
	rep := Reply{
		Snapshot: Snapshot{
			Schema: SchemaRequest, RequestRef: "amqr1_" + strings.Repeat("a", 20),
			RequestID:   "11111111-1111-4111-8111-111111111501",
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
