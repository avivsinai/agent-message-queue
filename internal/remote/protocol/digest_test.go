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
	// B10: Changing not_after does NOT change the digest. A deadline is
	// policy, not identity — a retry with a fresh deadline must match.
	changed = *base
	changed.NotAfter = "2026-09-08T11:00:00Z"
	if CommandDigest(&changed) != d {
		t.Fatal("digest changed after not_after change (B10 — deadline must not affect identity digest)")
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
	// B10: not_after is NOT in the digest payload.
	canonical := `{"schema":"` + string(SchemaCommand) + `","op":"` + string(OpRequestSubmit) +
		`","request_id":"11111111-1111-4111-8111-111111111501","target_id":"t_fake1","epoch":"e_1",` +
		`"input":{"text":"say hi","busy":"queue","deliver":"turn"}}`
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

// TestCommandDigestMinEvidenceIsIdentity pins the MinEvidence contract: the
// caller-stated evidence floor is part of the durable input digest, so it
// cannot be stripped for a retry against an older endpoint. An omitted floor
// (legacy) and a spelled floor are semantically DIFFERENT requests — one
// requires a guarantee the other does not — so they MUST digest differently.
// This is the protocol half of the Wave A.4 MinEvidence floor.
func TestCommandDigestMinEvidenceIsIdentity(t *testing.T) {
	base := &Command{
		Schema:    SchemaCommand,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111501",
		TargetID:  "t_fake1",
		Epoch:     "e_1",
		NotAfter:  "2026-09-08T10:02:00Z",
		Input:     &SubmitInput{Text: "say hi", Busy: BusyReject, Deliver: DeliverTurn},
	}
	baseDigest := CommandDigest(base) // no min_evidence: legacy

	// A spelled floor changes the digest: it cannot be stripped for retry.
	withFloor := *base
	withFloor.Input = &SubmitInput{Text: "say hi", Busy: BusyReject, Deliver: DeliverTurn, MinEvidence: EvidenceAdmitted}
	if CommandDigest(&withFloor) == baseDigest {
		t.Fatal("digest unchanged after adding min_evidence=admitted; the floor must be part of identity")
	}

	// Two different floors digest differently (admitted vs submitted).
	submitted := *base
	submitted.Input = &SubmitInput{Text: "say hi", Busy: BusyReject, Deliver: DeliverTurn, MinEvidence: EvidenceSubmitted}
	if CommandDigest(&submitted) == CommandDigest(&withFloor) {
		t.Fatal("admitted and submitted floors produce the same digest")
	}

	// The floor is stable: the same floor reproduces the same digest.
	again := withFloor
	if CommandDigest(&again) != CommandDigest(&withFloor) {
		t.Fatal("min_evidence digest not stable")
	}
}

// TestMinEvidenceValidation pins that Validate accepts the known classes and
// rejects an unknown floor clearly (a strict v1 peer fails instead of
// silently admitting work it cannot prove).
func TestMinEvidenceValidation(t *testing.T) {
	base := &Command{
		Schema:    SchemaCommand,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111501",
		TargetID:  "t_fake1",
		Epoch:     "e_1",
		NotAfter:  "2026-09-08T10:02:00Z",
		Input:     &SubmitInput{Text: "say hi"},
	}
	// Omitted (legacy) is valid.
	if err := base.Validate(); err != nil {
		t.Fatalf("omitted min_evidence rejected: %v", err)
	}
	for _, floor := range []string{EvidenceSubmitted, EvidenceAdmitted} {
		cmd := *base
		cmd.Input = &SubmitInput{Text: "say hi", MinEvidence: floor}
		if err := cmd.Validate(); err != nil {
			t.Fatalf("min_evidence=%s rejected: %v", floor, err)
		}
	}
	// Unknown floor is rejected.
	cmd := *base
	cmd.Input = &SubmitInput{Text: "say hi", MinEvidence: "guaranteed"}
	if err := cmd.Validate(); err == nil {
		t.Fatal("unknown min_evidence accepted; want invalid")
	}
}

// TestEvidenceClassMeets pins the within-kind ordering: admitted outranks
// submitted; an empty floor always passes (legacy); an unknown value never
// meets a non-empty floor (fail closed). The ranks are NOT comparable across
// evidence kinds.
func TestEvidenceClassMeets(t *testing.T) {
	cases := []struct {
		have, want string
		wantOK     bool
	}{
		// Empty floor: legacy, any evidence admitted.
		{have: "", want: "", wantOK: true},
		{have: EvidenceSubmitted, want: "", wantOK: true},
		{have: EvidenceAdmitted, want: "", wantOK: true},
		// Admitted meets submitted and admitted.
		{have: EvidenceAdmitted, want: EvidenceSubmitted, wantOK: true},
		{have: EvidenceAdmitted, want: EvidenceAdmitted, wantOK: true},
		// Submitted meets submitted but NOT admitted (weaker refused, not
		// substituted).
		{have: EvidenceSubmitted, want: EvidenceSubmitted, wantOK: true},
		{have: EvidenceSubmitted, want: EvidenceAdmitted, wantOK: false},
		// Unknown/empty evidence never meets a non-empty floor (fail closed).
		{have: "", want: EvidenceSubmitted, wantOK: false},
		{have: "guaranteed", want: EvidenceSubmitted, wantOK: false},
		{have: EvidenceSubmitted, want: "guaranteed", wantOK: false},
	}
	for _, c := range cases {
		if got := EvidenceClassMeets(c.have, c.want); got != c.wantOK {
			t.Fatalf("EvidenceClassMeets(have=%q, want=%q) = %v, want %v", c.have, c.want, got, c.wantOK)
		}
	}
}
