package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// linkAnswerFixture is the unit A chain machine with the binding's harness
// named claude: the hook seam that publishes and applies its dialogs is the
// Claude permission hook the tests drive in process.
func linkAnswerFixture(t *testing.T) (core.Source, *core.Endpoint) {
	t.Helper()
	_, _, linkSrc, ep := linkDialogChainFixture(t)
	b, err := binding.ReadNamed("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	b.Harness = "claude"
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	return linkSrc, ep
}

// runHookDialog starts one real Claude PermissionRequest hook and waits
// until its dialog record is complete in the binding's dialog directory.
// ended closes when the hook returns; out holds its decision.
func runHookDialog(t *testing.T) (reqID string, ended <-chan struct{}, out *bytes.Buffer) {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, _ := json.Marshal(map[string]any{
		"session_id": "pi:chat", "cwd": t.TempDir(), "transcript_path": transcript,
		"hook_event_name": "PermissionRequest", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "rm -rf node_modules", "description": "Remove node_modules"},
	})
	out = &bytes.Buffer{}
	hookDone := make(chan struct{})
	endedC := make(chan struct{})
	go func() {
		defer close(endedC)
		claude.RunPermissionHook(bytes.NewReader(stdin), out, hookDone, nil)
	}()
	ended = endedC
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		dir, err := dialog.DirRead("pi-chat")
		if err != nil {
			continue
		}
		reqs, _ := dialog.Requests(dir)
		if len(reqs) == 1 && reqs[0].Complete {
			return reqs[0].ID, ended, out
		}
	}
	t.Fatal("the hook wrote no dialog")
	return "", nil, nil
}

// linkSubmit admits the chain's running link request and returns its ref.
func linkSubmit(t *testing.T, ep *core.Endpoint, linkSrc core.Source) (string, string) {
	t.Helper()
	id := "11111111-1111-4111-8111-1111111111b2"
	if _, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "pi:chat", Epoch: "pi-session-1",
		NotAfter: protocol.FormatTime(time.Now().Add(10 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "x"}}, linkSrc); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return id, protocol.EncodeRef(linkSrc.Host, "pi:chat", id)
}

// linkDigest computes what the server signed: sha256 over the JCS bytes of
// the interaction's identity keys, the remote_answer flag excluded.
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

// waitResolved waits until the binding's dialog id carries a resolution.
func waitResolved(t *testing.T, id string) dialog.Resolved {
	t.Helper()
	dir, err := dialog.DirRead("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if r, ok, _ := dialog.ReadResolved(dir, id); ok {
			return r
		}
	}
	t.Fatal("the dialog never resolved")
	return dialog.Resolved{}
}

// The ratified chain, allow side: a real Claude hook event publishes the
// dialog with the button live, the verified link answer is admitted, the
// live hook returns the allow decision, and the resolution reads answered
// with the chosen label, by the link.
func TestLinkAnswerChainAllow(t *testing.T) {
	linkSrc, ep := linkAnswerFixture(t)
	id, ref := linkSubmit(t, ep, linkSrc)
	_ = id
	reqID, ended, out := runHookDialog(t)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap := requestGet(t, ep, linkSrc, ref)
	if snap.Interaction == nil || !snap.Interaction.RemoteAnswer {
		t.Fatalf("published interaction = %+v; want the button live", snap.Interaction)
	}
	want := snap.Interaction.ApproveOption
	a := core.LinkAnswer{
		RequestRef:        ref,
		InteractionID:     snap.Interaction.InteractionID,
		InteractionDigest: linkDigest(snap.Interaction),
		Option:            want,
		DocumentDigest:    linkDigest(snap.Interaction),
		Credential:        "k1",
	}
	if code, msg := ep.AnswerLinkDialog(linkSrc, a); code != "" {
		t.Fatalf("admission refused: %s %s", code, msg)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not end after the link answer")
	}
	var d struct {
		HookSpecificOutput struct {
			Decision struct {
				Behavior string `json:"behavior"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatalf("hook decision %q: %v", out.String(), err)
	}
	if d.HookSpecificOutput.Decision.Behavior != "allow" {
		t.Fatalf("hook decision = %s; want allow", out.String())
	}
	r := waitResolved(t, reqID)
	if r.Outcome != dialog.OutcomeAllowed || r.Label != snap.Interaction.ApproveOption || r.By != dialog.ByLink {
		t.Fatalf("resolution = %+v; want allowed %q by link", r, snap.Interaction.ApproveOption)
	}
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap = requestGet(t, ep, linkSrc, ref)
	if snap.Interaction != nil {
		t.Fatalf("resolved dialog still published: %+v", snap.Interaction)
	}
	found := false
	for _, res := range snap.Resolved {
		if res.InteractionID == reqID {
			found = true
			if res.Outcome != protocol.ResolutionAnswered || res.Option != want {
				t.Fatalf("resolved entry = %+v; want answered %q", res, want)
			}
		}
	}
	if !found {
		t.Fatalf("no resolved entry for %s in %+v", reqID, snap.Resolved)
	}
}

// The deny side: the reject label admits, the hook denies, the resolution
// reads denied with the label, by the link.
func TestLinkAnswerChainDeny(t *testing.T) {
	linkSrc, ep := linkAnswerFixture(t)
	_, ref := linkSubmit(t, ep, linkSrc)
	reqID, ended, out := runHookDialog(t)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap := requestGet(t, ep, linkSrc, ref)
	a := core.LinkAnswer{
		RequestRef:        ref,
		InteractionID:     snap.Interaction.InteractionID,
		InteractionDigest: linkDigest(snap.Interaction),
		Option:            snap.Interaction.RejectOption,
		DocumentDigest:    linkDigest(snap.Interaction),
		Credential:        "k1",
	}
	if code, msg := ep.AnswerLinkDialog(linkSrc, a); code != "" {
		t.Fatalf("admission refused: %s %s", code, msg)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not end after the link answer")
	}
	var d struct {
		HookSpecificOutput struct {
			Decision struct {
				Behavior string `json:"behavior"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatalf("hook decision %q: %v", out.String(), err)
	}
	if d.HookSpecificOutput.Decision.Behavior != "deny" {
		t.Fatalf("hook decision = %s; want deny", out.String())
	}
	r := waitResolved(t, reqID)
	if r.Outcome != dialog.OutcomeDenied || r.Label != snap.Interaction.RejectOption || r.By != dialog.ByLink {
		t.Fatalf("resolution = %+v; want denied %q by link", r, snap.Interaction.RejectOption)
	}
}

// A competing Buzz answer written first wins: the link's send gets
// already_resolved, and the outcome reads answered_elsewhere.
func TestLinkAnswerCompetingBuzzAnswerWins(t *testing.T) {
	linkSrc, ep := linkAnswerFixture(t)
	_, ref := linkSubmit(t, ep, linkSrc)
	reqID, ended, _ := runHookDialog(t)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap := requestGet(t, ep, linkSrc, ref)
	dir, err := dialog.DirFor("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: reqID, Option: 1, By: dialog.ByBuzz, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	a := core.LinkAnswer{
		RequestRef:        ref,
		InteractionID:     snap.Interaction.InteractionID,
		InteractionDigest: linkDigest(snap.Interaction),
		Option:            snap.Interaction.ApproveOption,
		DocumentDigest:    linkDigest(snap.Interaction),
		Credential:        "k1",
	}
	code, _ := ep.AnswerLinkDialog(linkSrc, a)
	if code != protocol.CodeAlreadyResolved {
		t.Fatalf("late link answer = %s; want already_resolved", code)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not end after the Buzz answer")
	}
	r := waitResolved(t, reqID)
	if r.By != dialog.ByBuzz {
		t.Fatalf("resolution by = %q; want buzz", r.By)
	}
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap = requestGet(t, ep, linkSrc, ref)
	found := false
	for _, res := range snap.Resolved {
		if res.InteractionID == reqID {
			found = true
			if res.Outcome != protocol.ResolutionElsewhere {
				t.Fatalf("resolved entry = %+v; want answered_elsewhere", res)
			}
		}
	}
	if !found {
		t.Fatalf("no resolved entry for %s in %+v", reqID, snap.Resolved)
	}
}

// The revision tracks the answer file: the button goes dark in a new
// revision of the same card while an answer waits, and lights again when
// the seam removes the answer as not valid.
func TestLinkAnswerRevisionTracksAnswerFile(t *testing.T) {
	linkSrc, ep := linkAnswerFixture(t)
	_, ref := linkSubmit(t, ep, linkSrc)

	// A dialog record without a hook: the seam under test is publication.
	dir, err := dialog.DirFor("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	reqID := dialog.NewID()
	if err := dialog.Open(dir, dialog.Request{ID: reqID, Harness: "claude", Session: "pi:chat",
		Tool: "Bash", Text: "echo hi", Complete: true, OpenedAt: time.Now().UTC(),
		Options: []dialog.Option{
			{N: 1, Label: "Allow once", Value: "allow", Scope: dialog.ScopeOnce},
			{N: 2, Label: "Deny", Value: "deny", Deny: true, Scope: dialog.ScopeOnce},
		}}); err != nil {
		t.Fatal(err)
	}
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap := requestGet(t, ep, linkSrc, ref)
	if snap.Interaction == nil || !snap.Interaction.RemoteAnswer {
		t.Fatalf("published = %+v; want the button live", snap.Interaction)
	}
	rev := snap.Revision

	// An answer file — the owner's from Buzz — takes the button dark in a
	// new revision of the same card.
	if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: reqID, Option: 2, By: dialog.ByBuzz, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap = requestGet(t, ep, linkSrc, ref)
	if snap.Interaction == nil || snap.Interaction.RemoteAnswer {
		t.Fatalf("after the answer = %+v; want the same card, button dark", snap.Interaction)
	}
	if snap.Interaction.InteractionID != reqID || snap.Revision <= rev {
		t.Fatalf("revision = %d (was %d), id %s", snap.Revision, rev, snap.Interaction.InteractionID)
	}

	// The seam removes the answer as not valid: the dialog is open again
	// and the button lights again.
	if err := dialog.RejectAnswer(dir, reqID); err != nil {
		t.Fatal(err)
	}
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap = requestGet(t, ep, linkSrc, ref)
	if snap.Interaction == nil || !snap.Interaction.RemoteAnswer {
		t.Fatalf("after the reject = %+v; want the button live again", snap.Interaction)
	}
}

// A seam that removes the answer as not valid reopens the dialog, and the
// same signed bytes are admitted again: the owner signed that choice for
// that dialog.
func TestLinkAnswerReopensAfterSeamReject(t *testing.T) {
	linkSrc, ep := linkAnswerFixture(t)
	_, ref := linkSubmit(t, ep, linkSrc)
	reqID, ended, _ := runHookDialog(t)
	_ = reqID
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap := requestGet(t, ep, linkSrc, ref)
	a := core.LinkAnswer{
		RequestRef:        ref,
		InteractionID:     snap.Interaction.InteractionID,
		InteractionDigest: linkDigest(snap.Interaction),
		Option:            snap.Interaction.ApproveOption,
		DocumentDigest:    linkDigest(snap.Interaction),
		Credential:        "k1",
	}
	if code, msg := ep.AnswerLinkDialog(linkSrc, a); code != "" {
		t.Fatalf("first admission refused: %s %s", code, msg)
	}
	dir, err := dialog.DirRead("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := dialog.RejectAnswer(dir, a.InteractionID); err != nil {
		t.Fatal(err)
	}
	phase, _, _, _, err := dialog.Lookup(dir, a.InteractionID)
	if err != nil || phase != dialog.PhaseOpen {
		t.Fatalf("phase after the seam reject = %s (%v); want open", phase, err)
	}
	if code, msg := ep.AnswerLinkDialog(linkSrc, a); code != "" {
		t.Fatalf("second admission refused: %s %s", code, msg)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not end after the second answer")
	}
}

// The directories exist as they are: a missing answers directory refuses
// with already_resolved and nothing is created.
func TestLinkAnswerMissingAnswersDirRefuses(t *testing.T) {
	linkSrc, ep := linkAnswerFixture(t)
	_, ref := linkSubmit(t, ep, linkSrc)
	reqID, ended, _ := runHookDialog(t)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	snap := requestGet(t, ep, linkSrc, ref)
	a := core.LinkAnswer{
		RequestRef:        ref,
		InteractionID:     snap.Interaction.InteractionID,
		InteractionDigest: linkDigest(snap.Interaction),
		Option:            snap.Interaction.ApproveOption,
		DocumentDigest:    linkDigest(snap.Interaction),
		Credential:        "k1",
	}
	dir, err := dialog.DirRead("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "answers")); err != nil {
		t.Fatal(err)
	}
	code, _ := ep.AnswerLinkDialog(linkSrc, a)
	if code != protocol.CodeAlreadyResolved {
		t.Fatalf("missing answers dir = %s; want already_resolved", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "answers")); !os.IsNotExist(err) {
		t.Fatalf("answers directory recreated: %v", err)
	}
	// The dialog files stay exactly as they were.
	if fi, err := os.Stat(filepath.Join(dir, "requests", reqID+".json")); err != nil || fi == nil {
		t.Fatalf("request file gone: %v", err)
	}
	_ = ended // the hook keeps waiting; the test does not need its decision
}

// requestGet is the linked server's read of one running request.
func requestGet(t *testing.T, ep *core.Endpoint, linkSrc core.Source, ref string) protocol.Snapshot {
	t.Helper()
	rep, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}, linkSrc)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return rep.(protocol.Reply).Snapshot
}
