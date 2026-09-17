package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestMinEvidenceFloorRefusesWeakerAdapter is the Wave A.4 behavior test: a
// caller that states min_evidence=admitted is REFUSED by an adapter whose
// strongest submit evidence is only "submitted" (weaker capability is
// refused, not substituted — ADR invariant 5). The check happens BEFORE side
// effects: no record is created, no dispatch attempted. An omitted floor
// preserves legacy semantics (any evidence admitted).
func TestMinEvidenceFloorRefusesWeakerAdapter(t *testing.T) {
	store, now := openStore(t)
	ep := core.New(core.Config{Store: store, Now: now})
	t.Cleanup(func() { _ = ep.Close() })

	// A weak-evidence adapter: it can submit but only proves "submitted",
	// not "admitted". This mirrors Amit's evidence class.
	rt := fake.New("fake", "e_1").WithEvidence(&protocol.Evidence{Submit: protocol.EvidenceSubmitted, Completion: "run_terminal"})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111115a4"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "do the thing", MinEvidence: protocol.EvidenceAdmitted},
	}
	repAny, err := ep.Handle(cmd, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("submit returned error (refusal is in Outcome.Code, not err): %v", err)
	}
	rep := repAny.(protocol.Reply)
	if rep.Outcome.Code != protocol.CodeUnsupported {
		t.Fatalf("outcome code=%s, want unsupported (weaker capability refused); state=%s", rep.Outcome.Code, rep.Snapshot.State)
	}
	if rep.Snapshot.State != protocol.StateRejected {
		t.Fatalf("state=%s, want rejected", rep.Snapshot.State)
	}
	// No side effect: no record was created.
	_, exists, _ := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if exists {
		t.Fatal("record was created despite evidence refusal")
	}
}

// TestMinEvidenceFloorAdmitsStrongerAdapter proves the floor passes when the
// adapter's evidence meets or exceeds the caller's requirement: a caller
// requiring "submitted" is admitted by an adapter that proves "admitted".
func TestMinEvidenceFloorAdmitsStrongerAdapter(t *testing.T) {
	store, now := openStore(t)
	ep := core.New(core.Config{Store: store, Now: now})
	t.Cleanup(func() { _ = ep.Close() })

	// Default fake proves submit=admitted (the strongest class).
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111115a5"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "do the thing", MinEvidence: protocol.EvidenceSubmitted},
	}
	if _, err := ep.Handle(cmd, core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit with min_evidence=submitted against an admitted adapter failed: %v", err)
	}
}

// TestMinEvidenceOmittedIsLegacy proves an omitted floor preserves legacy
// semantics: a weak-evidence adapter is admitted when no floor is stated.
func TestMinEvidenceOmittedIsLegacy(t *testing.T) {
	store, now := openStore(t)
	ep := core.New(core.Config{Store: store, Now: now})
	t.Cleanup(func() { _ = ep.Close() })

	rt := fake.New("fake", "e_1").WithEvidence(&protocol.Evidence{Submit: protocol.EvidenceSubmitted, Completion: "run_terminal"})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111115a6"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "do the thing"}, // no min_evidence
	}
	if _, err := ep.Handle(cmd, core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit with omitted min_evidence against a weak adapter failed (legacy should admit): %v", err)
	}
}
