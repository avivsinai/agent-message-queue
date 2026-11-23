package core_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// answerFixture admits one running link request on one shared binding and
// publishes a dialog interaction for it, the state the answer checks read.
func answerFixture(t *testing.T) (*core.Endpoint, *requests.Store, *stubDialogs, requests.Key, *protocol.Interaction, core.Source) {
	t.Helper()
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return nil, nil })
	stub := &stubDialogs{}
	ep.SetLinkDialogs(stub)

	id := "11111111-1111-4111-8111-1111111111d5"
	src := linkShare("pi-chat")
	if _, err := ep.Handle(submitCmd(id), src); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(func(*requests.Record) *protocol.Interaction {
		return &protocol.Interaction{
			InteractionID: "d-0000000000000000000000000000aaaa",
			Kind:          "approval",
			Prompt:        "Bash\n$ go test ./...",
			Options:       []string{"Allow once", "Deny"}, RemoteAnswer: true,
			ApproveOption: "Allow once",
			RejectOption:  "Deny",
		}
	})
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}
	rec, exists, err := store.Get(key)
	if err != nil || !exists || rec.Interaction == nil || !rec.Interaction.RemoteAnswer {
		t.Fatalf("fixture: interaction not published (%v, %+v)", exists, rec.Interaction)
	}
	return ep, store, stub, key, rec.Interaction, src
}

// linkAnswer builds the admission input for one interaction: the digest of
// exactly the identity keys, as the server signed it.
func linkAnswer(ref string, in *protocol.Interaction, option string) core.LinkAnswer {
	return core.LinkAnswer{
		RequestRef:        ref,
		InteractionID:     in.InteractionID,
		InteractionDigest: linkDigest(in),
		Option:            option,
		DocumentDigest:    linkDigest(in),
		Credential:        "k1",
	}
}

// linkDigest is the value the server signs: "sha256:" + hex of the JCS
// bytes of the interaction's identity — exactly interaction_id, kind,
// options, prompt and, when present, approve_option and reject_option. The
// remote_answer flag is part of the revision, never of the digest, so the
// test strips it before marshalling.
func linkDigest(in *protocol.Interaction) string {
	cp := *in
	cp.RemoteAnswer = false
	canon, err := jcs.Marshal(cp)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// The machine admits a verified answer: the write reaches the seam with the
// signed fields, and returns "" once durable.
func TestAnswerLinkDialogAdmitsVerifiedAnswer(t *testing.T) {
	ep, _, stub, key, in, src := answerFixture(t)
	ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
	if code, msg := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption)); code != "" {
		t.Fatalf("admission refused: %s %s", code, msg)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.answers) != 1 || stub.answers[0] != "pi-chat/"+in.InteractionID+"/Allow once/approve" {
		t.Fatalf("seam write = %v; want the binding, id, approve label and role", stub.answers)
	}
}

// The digest excludes the remote_answer flag: a document signed while the
// button was lit still matches after the flag drops (an answer file
// appeared) — and only that flag, not a moved prompt, may be excluded.
func TestAnswerLinkDialogDigestExcludesRemoteAnswerFlagOnly(t *testing.T) {
	ep, _, _, key, in, src := answerFixture(t)
	ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
	a := linkAnswer(ref, in, in.ApproveOption)
	// The same document with the flag's other value still matches.
	withFlag := *in
	withFlag.RemoteAnswer = false
	a.InteractionDigest = linkDigest(&withFlag)
	if code, _ := ep.AnswerLinkDialog(src, a); code != "" {
		t.Fatalf("flag-stripped digest refused: %s", code)
	}
	// A moved prompt stales it, on a fresh dialog: the first admission
	// recorded an answer, and a recorded answer wins ahead of the digest.
	ep, _, _, key, in, src = answerFixture(t)
	ref = protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
	moved := *in
	moved.Prompt = "Bash\n$ wipe everything"
	a = linkAnswer(ref, &moved, moved.ApproveOption)
	if code, _ := ep.AnswerLinkDialog(src, a); code != protocol.CodeDialogChanged {
		t.Fatalf("moved dialog = %s; want dialog_changed", code)
	}
}

// Each refusal of the interface, in the contract's order.
func TestAnswerLinkDialogRefusalsInOrder(t *testing.T) {
	t.Run("answer_unsupported before everything", func(t *testing.T) {
		ep, _, stub, key, in, src := answerFixture(t)
		stub.mu.Lock()
		stub.policy = func(string) (bool, string) { return false, "consent_local" }
		stub.mu.Unlock()
		ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
		code, msg := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption))
		if code != protocol.CodeAnswerUnsupported || msg != "consent_local" {
			t.Fatalf("refusal = %s %s; want answer_unsupported consent_local", code, msg)
		}
		// A source with no binding reads the same way, before any record
		// lookup.
		src2 := src
		src2.Binding = ""
		if code, _ := ep.AnswerLinkDialog(src2, linkAnswer(ref, in, in.ApproveOption)); code != protocol.CodeAnswerUnsupported {
			t.Fatalf("empty binding refusal = %s; want answer_unsupported", code)
		}
	})
	t.Run("not_found: another link's ref, a stopped record, another binding, a bad ref", func(t *testing.T) {
		ep, store, _, key, in, src := answerFixture(t)
		ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
		// Another host's ref.
		otherRef := protocol.EncodeRef("link-y", key.TargetID, key.RequestID)
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(otherRef, in, in.ApproveOption)); code != protocol.CodeNotFound {
			t.Fatalf("foreign ref = %s; want not_found", code)
		}
		// Not running.
		rec, _, _ := store.Get(key)
		rec.State = protocol.StateCompleted
		if err := store.WriteMemo(rec); err != nil {
			t.Fatal(err)
		}
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption)); code != protocol.CodeNotFound {
			t.Fatalf("stopped record = %s; want not_found", code)
		}
		// Running again, but the source names another binding.
		rec.State = protocol.StateRunning
		if err := store.WriteMemo(rec); err != nil {
			t.Fatal(err)
		}
		src2 := src
		src2.Binding = "other-chat"
		if code, _ := ep.AnswerLinkDialog(src2, linkAnswer(ref, in, in.ApproveOption)); code != protocol.CodeNotFound {
			t.Fatalf("binding mismatch = %s; want not_found", code)
		}
		// Undecodable ref.
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer("bogus-ref", in, in.ApproveOption)); code != protocol.CodeNotFound {
			t.Fatalf("bad ref = %s; want not_found", code)
		}
	})
	t.Run("already_resolved: no interaction, a foreign id, remote_answer false", func(t *testing.T) {
		ep, store, stub, key, in, src := answerFixture(t)
		ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
		// No pending interaction.
		rec, _, _ := store.Get(key)
		rec.Interaction = nil
		if err := store.WriteMemo(rec); err != nil {
			t.Fatal(err)
		}
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption)); code != protocol.CodeAlreadyResolved {
			t.Fatalf("no interaction = %s; want already_resolved", code)
		}
		// A pinned interaction the record never selected.
		rec.Interaction = in
		rec.LinkDialogs = nil
		if err := store.WriteMemo(rec); err != nil {
			t.Fatal(err)
		}
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption)); code != protocol.CodeAlreadyResolved {
			t.Fatalf("unselected id = %s; want already_resolved", code)
		}
		// remote_answer false with an answer file: the file is the truth
		// about "already answered" — the first answer wins.
		rec.LinkDialogs = []string{in.InteractionID}
		rec.Interaction = in
		rec.Interaction.RemoteAnswer = false
		if err := store.WriteMemo(rec); err != nil {
			t.Fatal(err)
		}
		stub.mu.Lock()
		stub.taken = map[string]bool{in.InteractionID: true}
		stub.mu.Unlock()
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption)); code != protocol.CodeAlreadyResolved {
			t.Fatalf("answered file, flag false = %s; want already_resolved", code)
		}
		// The stored flag reads false but the answer file is gone — the
		// seam removed a bad answer before the next reconcile: the call is
		// admitted, the flag is never the truth.
		stub.mu.Lock()
		stub.taken = nil
		stub.mu.Unlock()
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption)); code != "" {
			t.Fatalf("stale dark flag with no answer file = %s; want admitted", code)
		}
	})
	t.Run("already_resolved: a recorded answer wins ahead of digest and option", func(t *testing.T) {
		ep, _, stub, key, in, src := answerFixture(t)
		ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
		stub.mu.Lock()
		// An answer file the stale button missed: HasAnswer true, the
		// published flag still lit, digest and option both wrong.
		stub.taken = map[string]bool{in.InteractionID: true}
		stub.mu.Unlock()
		a := linkAnswer(ref, in, in.RejectOption)
		a.InteractionDigest = "sha256:" + hex.EncodeToString([]byte("stale"))
		if code, _ := ep.AnswerLinkDialog(src, a); code != protocol.CodeAlreadyResolved {
			t.Fatalf("recorded answer with stale flag = %s; want already_resolved", code)
		}
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if len(stub.answers) != 0 {
			t.Fatalf("seam wrote again: %v", stub.answers)
		}
	})
	t.Run("dialog_changed: the digest of a moved dialog", func(t *testing.T) {
		ep, _, _, key, in, src := answerFixture(t)
		ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
		a := linkAnswer(ref, in, in.ApproveOption)
		a.InteractionDigest = "sha256:" + hex.EncodeToString([]byte("stale"))
		if code, _ := ep.AnswerLinkDialog(src, a); code != protocol.CodeDialogChanged {
			t.Fatalf("stale digest = %s; want dialog_changed", code)
		}
	})
	t.Run("invalid: an option outside the two roles", func(t *testing.T) {
		ep, _, _, key, in, src := answerFixture(t)
		ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, "Always allow")); code != protocol.CodeInvalid {
			t.Fatalf("foreign label = %s; want invalid", code)
		}
		// An empty label is no choice either.
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, "")); code != protocol.CodeInvalid {
			t.Fatalf("empty label = %s; want invalid", code)
		}
	})
	t.Run("the seam's refusals pass through", func(t *testing.T) {
		ep, _, stub, key, in, src := answerFixture(t)
		ref := protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
		stub.mu.Lock()
		stub.write = func() (protocol.Code, string) { return protocol.CodeAlreadyResolved, "an answer exists already" }
		stub.mu.Unlock()
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption)); code != protocol.CodeAlreadyResolved {
			t.Fatalf("seam already_resolved = %s", code)
		}
		stub.mu.Lock()
		stub.taken = nil // the recorded answer is out of the way for the next row
		stub.write = func() (protocol.Code, string) { return protocol.CodeInternal, "directory sync failed" }
		stub.mu.Unlock()
		if code, _ := ep.AnswerLinkDialog(src, linkAnswer(ref, in, in.ApproveOption)); code != protocol.CodeInternal {
			t.Fatalf("seam internal = %s", code)
		}
	})
}

// LinkAnswerInfo reports the seam's policy and its reason, and no seam or
// no binding reads as unavailable.
func TestLinkAnswerInfoReportsPolicy(t *testing.T) {
	ep, _, stub, _, _, src := answerFixture(t)
	if available, reason := ep.LinkAnswerInfo(src, "pi-chat"); !available || reason != "" {
		t.Fatalf("policy = %v %q; want available", available, reason)
	}
	stub.mu.Lock()
	stub.policy = func(string) (bool, string) { return false, "harness_unsupported" }
	stub.mu.Unlock()
	if available, reason := ep.LinkAnswerInfo(src, "pi-chat"); available || reason != "harness_unsupported" {
		t.Fatalf("policy = %v %q; want harness_unsupported", available, reason)
	}
	if _, reason := ep.LinkAnswerInfo(src, ""); reason != "approvals_terminal" {
		t.Fatalf("empty binding reason = %q", reason)
	}
}
