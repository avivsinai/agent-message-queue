package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bytes"
	"encoding/json"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// linkDialogFixture is one live pi binding, one running link sharing it with
// approvals=link and passkey consent, and one record admitted against that
// binding with a captured baseline.
func linkDialogFixture(t *testing.T, nativeSession string, baseline []string) (*linkDialogSource, *requests.Record) {
	t.Helper()
	_, ls := restoreLinkTestBinding(t, time.Now())
	// A real carrier keyed like newRun builds one, so byHost can resolve
	// the owning link by creator host.
	stateDir := filepath.Join(ls.root, stateDirName)
	// The fake's native id is its target, so name the binding's native
	// session what the endpoint reports and what the record signs.
	b, err := binding.ReadNamed("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	b.NativeSession = nativeSession
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	if _, err := linkio.MintDeviceKey(stateDir, "link"); err != nil {
		t.Fatal(err)
	}
	if err := linkio.WritePin(stateDir, "link", linkio.Pin{ServerID: "srv_linkdlg"}); err != nil {
		t.Fatal(err)
	}
	key, err := linkio.LoadDeviceKey(stateDir, "link")
	if err != nil {
		t.Fatal(err)
	}
	pin, err := linkio.LoadPin(stateDir, "link")
	if err != nil {
		t.Fatal(err)
	}
	pin.URL = "wss://linkdlg.invalid/link"
	storeID, err := linkio.StoreID(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := linkio.New(linkio.Config{Name: "link", Pin: pin, Key: key, StoreID: storeID, Bindings: func() []linkio.Binding { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	run := ls.running["link"]
	run.carrier = carrier
	if ls.byHost == nil {
		ls.byHost = map[string]*linkio.Carrier{}
	}
	ls.byHost[carrier.Host()] = carrier
	run.link.Shares[0].Consent = "passkey"
	run.link.Shares[0].Approvals = manifest.ApprovalsLink
	src := &linkDialogSource{links: ls}
	rec := &requests.Record{Snapshot: protocol.Snapshot{State: protocol.StateRunning}, Binding: "pi-chat"}
	rec.CreatorHost = carrier.Host()
	rec.NativeRun = func() *string { s := "pi-session-1"; return &s }()
	rec.TargetID = "pi:chat"
	rec.Epoch = "pi-session-1"
	rec.NativeSession = nativeSession
	if baseline != nil {
		rec.DialogsBefore = baseline
	}
	return src, rec
}

// openDialog writes one complete dialog request into the binding's dialog
// directory.
func openDialog(t *testing.T, bindingName string, req dialog.Request) {
	t.Helper()
	req.Complete = true
	writeDialog(t, bindingName, req)
}

// openIncomplete writes one dialog still awaiting its answer.
func openIncomplete(t *testing.T, bindingName string, req dialog.Request) {
	t.Helper()
	req.Complete = false
	writeDialog(t, bindingName, req)
}

func writeDialog(t *testing.T, bindingName string, req dialog.Request) {
	t.Helper()
	dir, err := dialog.DirFor(bindingName)
	if err != nil {
		t.Fatal(err)
	}
	if req.ID == "" {
		req.ID = dialog.NewID()
	}
	if req.OpenedAt.IsZero() {
		req.OpenedAt = time.Now()
	}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
}

var allowOnce = dialog.Option{N: 1, Label: "Allow once", Value: "allow", Scope: dialog.ScopeOnce}
var denyOnce = dialog.Option{N: 2, Label: "Deny", Value: "deny", Deny: true, Scope: dialog.ScopeOnce}

// linkDialogChainFixture is the full machine face for the end-to-end
// chain: a live pi binding, a running link sharing it with approvals=link
// and passkey consent, and an endpoint with the production selector
// installed.
func linkDialogChainFixture(t *testing.T) (*linkSet, *linkDialogSource, core.Source, *core.Endpoint) {
	t.Helper()
	_, ls := restoreLinkTestBinding(t, time.Now())
	// The fake's native id is its target, so name the binding's native
	// session what the endpoint reports and what the hook's session names.
	b, err := binding.ReadNamed("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	b.NativeSession = "pi:chat"
	// The restore helper deliberately ends its binding's holder; the hook
	// path needs a live one, so bind the holder to this process.
	if holder, err := launch.HolderOf(os.Getpid()); err == nil {
		b.Holder = holder
	}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(ls.root, stateDirName)
	if _, err := linkio.MintDeviceKey(stateDir, "link"); err != nil {
		t.Fatal(err)
	}
	if err := linkio.WritePin(stateDir, "link", linkio.Pin{ServerID: "srv_linkdlg"}); err != nil {
		t.Fatal(err)
	}
	key, err := linkio.LoadDeviceKey(stateDir, "link")
	if err != nil {
		t.Fatal(err)
	}
	pin, err := linkio.LoadPin(stateDir, "link")
	if err != nil {
		t.Fatal(err)
	}
	pin.URL = "wss://linkdlg.invalid/link"
	storeID, err := linkio.StoreID(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := linkio.New(linkio.Config{Name: "link", Pin: pin, Key: key, StoreID: storeID, Bindings: func() []linkio.Binding { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	run := ls.running["link"]
	run.carrier = carrier
	if ls.byHost == nil {
		ls.byHost = map[string]*linkio.Carrier{}
	}
	ls.byHost[carrier.Host()] = carrier
	run.link.Shares[0].Consent = "passkey"
	run.link.Shares[0].Approvals = manifest.ApprovalsLink
	ep := ls.ep
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return []string{}, nil })
	src := &linkDialogSource{links: ls}
	ep.SetLinkDialogs(src)
	linkHost := carrier.Host()
	linkSrc := core.Source{Host: linkHost, Origin: map[string]string{"carrier": core.CarrierLink, core.OriginSink: linkHost},
		Shared: []string{"pi:chat"}, NativeSession: "pi:chat", Credential: "k1", Binding: "pi-chat"}
	return ls, src, linkSrc, ep
}

// The ratified happy path, end to end: a real Claude PermissionRequest
// hook event writes the dialog, the production machine selector publishes
// it against the record's signed session, request.get shows it to the
// linked server, a Buzz answer resolves it through the hook, and the next
// get shows the disposition with the revision moved.
func TestLinkDialogHookToSelectorToRequestGet(t *testing.T) {
	_, _, linkSrc, ep := linkDialogChainFixture(t)

	id := "11111111-1111-4111-8111-1111111111e2"
	if _, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "pi:chat", Epoch: "pi-session-1",
		NotAfter: protocol.FormatTime(time.Now().Add(10 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "x"}}, linkSrc); err != nil {
		t.Fatalf("submit: %v", err)
	}
	ref := protocol.EncodeRef(linkSrc.Host, "pi:chat", id)
	get := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}

	// The real hook: one PermissionRequest event for the session the
	// binding serves.
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, _ := json.Marshal(map[string]any{
		"session_id": "pi:chat", "cwd": t.TempDir(), "transcript_path": transcript,
		"hook_event_name": "PermissionRequest", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "rm -rf node_modules", "description": "Remove node_modules"},
	})
	var out bytes.Buffer
	hookDone := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		claude.RunPermissionHook(bytes.NewReader(stdin), &out, hookDone, nil)
	}()

	// The hook mirrors the dialog into the binding's dialog directory.
	var reqID string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		dir, err := dialog.DirRead("pi-chat")
		if err != nil {
			continue
		}
		reqs, _ := dialog.Requests(dir)
		if len(reqs) == 1 && reqs[0].Complete {
			reqID = reqs[0].ID
			break
		}
	}
	if reqID == "" {
		t.Fatal("the hook wrote no dialog")
	}

	// The production selector publishes it; the linked server sees it.
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rep, err := ep.Handle(get, linkSrc)
	if err != nil {
		t.Fatalf("get after publication: %v", err)
	}
	snap := rep.(protocol.Reply).Snapshot
	if snap.Interaction == nil || snap.Interaction.InteractionID != reqID {
		t.Fatalf("snapshot interaction = %+v; want the hook's dialog %s", snap.Interaction, reqID)
	}
	if snap.Interaction.RemoteAnswer {
		t.Fatal("unit A published remote_answer")
	}
	if snap.Interaction.ApproveOption != "Allow once" || snap.Interaction.RejectOption != "Deny" {
		t.Fatalf("roles = %q/%q", snap.Interaction.ApproveOption, snap.Interaction.RejectOption)
	}
	revPublished := snap.Revision

	// The machine's Buzz agent answers option 1; the hook applies it and
	// writes the decision.
	dir, err := dialog.DirFor("pi-chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: reqID, Option: 1, By: dialog.ByBuzz, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not end after the answer")
	}
	var d struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatalf("hook decision %q: %v", out.String(), err)
	}
	if d.HookSpecificOutput.HookEventName != "PermissionRequest" || d.HookSpecificOutput.Decision.Behavior != "allow" {
		t.Fatalf("hook decision = %s; want a PermissionRequest allow", out.String())
	}

	// The disposition reaches the linked server through the next get.
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rep, err = ep.Handle(get, linkSrc)
	if err != nil {
		t.Fatalf("get after resolution: %v", err)
	}
	snap = rep.(protocol.Reply).Snapshot
	if snap.Interaction != nil {
		t.Fatalf("snapshot kept the resolved interaction: %+v", snap.Interaction)
	}
	var got *protocol.Resolution
	for i := range snap.Resolved {
		if snap.Resolved[i].InteractionID == reqID {
			got = &snap.Resolved[i]
		}
	}
	if got == nil || got.Outcome != protocol.ResolutionElsewhere || got.Option != "Allow once" {
		t.Fatalf("resolved = %+v; want elsewhere/Allow once", got)
	}
	if snap.Revision <= revPublished {
		t.Fatalf("revision %d after resolution; want above %d", snap.Revision, revPublished)
	}
}

// Only the exact running link, with the share naming the binding, passkey
// consent and approvals=link, publishes: terminal approvals and local
// consent publish nothing, and a record without a signed session publishes
// nothing.
func TestLinkDialogPolicyGatesSelection(t *testing.T) {
	src, rec := linkDialogFixture(t, "pi:chat", []string{})
	run := src.links.running["link"]

	rec.NativeSession = ""
	openDialog(t, "pi-chat", dialog.Request{ID: dialog.NewID(), Harness: "pi", Tool: "Bash", Text: "ls", Session: "pi:chat", Options: []dialog.Option{allowOnce, denyOnce}})
	if src.Select(rec) != nil {
		t.Fatal("a record with no signed session published a dialog")
	}
	rec.NativeSession = "pi:chat"

	run.link.Shares[0].Approvals = manifest.ApprovalsTerminal
	if src.Select(rec) != nil {
		t.Fatal("terminal approvals published a dialog")
	}
	run.link.Shares[0].Approvals = manifest.ApprovalsLink
	run.link.Shares[0].Consent = "local"
	if src.Select(rec) != nil {
		t.Fatal("local consent published a dialog")
	}
	run.link.Shares[0].Consent = "passkey"

	in := src.Select(rec)
	if in == nil {
		t.Fatal("a gated-in dialog published nothing")
	}
	if in.RemoteAnswer {
		t.Fatal("unit A published remote_answer")
	}
	if in.ApproveOption != "Allow once" || in.RejectOption != "Deny" {
		t.Fatalf("roles = %q/%q; want the once options", in.ApproveOption, in.RejectOption)
	}
}

// R1: a record without a captured baseline publishes nothing on either
// path; an empty baseline is a real one.
func TestLinkDialogNilBaselineWithholds(t *testing.T) {
	src, rec := linkDialogFixture(t, "pi:chat", nil)
	openDialog(t, "pi-chat", dialog.Request{ID: dialog.NewID(), Harness: "pi", Tool: "Bash", Text: "ls", Session: "pi:chat", Options: []dialog.Option{allowOnce}})
	if src.Select(rec) != nil {
		t.Fatal("a nil baseline published a dialog")
	}
	rec.DialogsBefore = []string{}
	if src.Select(rec) == nil {
		t.Fatal("an empty baseline withheld a real dialog")
	}
}

// A dialog of the run's start baseline is never the run's dialog; an
// incomplete one never publishes; the earliest fully eligible dialog wins.
func TestLinkDialogSelectionOrderAndFilters(t *testing.T) {
	baseID := dialog.NewID()
	src, rec := linkDialogFixture(t, "pi:chat", []string{baseID})
	early := dialog.Request{ID: dialog.NewID(), Harness: "pi", Tool: "Bash", Text: "early", Session: "pi:chat", OpenedAt: time.Now().Add(-time.Minute), Options: []dialog.Option{allowOnce}}
	openIncomplete(t, "pi-chat", early)
	base := dialog.Request{ID: baseID, Harness: "pi", Tool: "Bash", Text: "baseline", Session: "pi:chat", OpenedAt: time.Now().Add(-2 * time.Minute), Options: []dialog.Option{allowOnce}}
	openDialog(t, "pi-chat", base)
	late := dialog.Request{ID: dialog.NewID(), Harness: "pi", Tool: "Bash", Text: "late", Session: "pi:chat", OpenedAt: time.Now(), Options: []dialog.Option{allowOnce}}
	openDialog(t, "pi-chat", late)

	in := src.Select(rec)
	if in == nil || in.InteractionID != late.ID {
		t.Fatalf("selection = %+v; want the earliest fully eligible dialog %s", in, late.ID)
	}
	if in.Prompt != "Bash\nlate" {
		t.Fatalf("prompt = %q; want the dialog's own text", in.Prompt)
	}
}

// A dialog of another native session, and one whose identity cannot be
// correlated, stays on the machine.
func TestLinkDialogForeignSessionWithheld(t *testing.T) {
	src, rec := linkDialogFixture(t, "pi:chat", []string{})
	openDialog(t, "pi-chat", dialog.Request{ID: dialog.NewID(), Harness: "pi", Tool: "Bash", Text: "x", Session: "other-session", Options: []dialog.Option{allowOnce}})
	if src.Select(rec) != nil {
		t.Fatal("another session's dialog published")
	}
	// A dialog with no session at all cannot be correlated.
	openDialog(t, "pi-chat", dialog.Request{ID: dialog.NewID(), Harness: "pi", Tool: "Bash", Text: "x", Options: []dialog.Option{allowOnce}})
	if src.Select(rec) != nil {
		t.Fatal("an uncorrelatable dialog published")
	}
}

// Only a legal choice set renders: two once-allows, no once options, or a
// colliding deny label each withhold.
func TestLinkDialogIllegalChoiceSetsWithhold(t *testing.T) {
	src, rec := linkDialogFixture(t, "pi:chat", []string{})
	always := dialog.Option{N: 1, Label: "Allow always", Value: "always", Scope: dialog.ScopeAlways}
	cases := [][]dialog.Option{
		{allowOnce, allowOnce}, // two once-allows
		{always},               // nothing once
		{allowOnce, {N: 2, Label: "Allow once", Value: "deny", Deny: true, Scope: dialog.ScopeOnce}}, // colliding labels
	}
	for _, opts := range cases {
		openDialog(t, "pi-chat", dialog.Request{ID: dialog.NewID(), Harness: "pi", Tool: "Bash", Text: "x", Session: "pi:chat", Options: opts})
		if src.Select(rec) != nil {
			t.Fatalf("illegal choice set %v published", opts)
		}
	}
}

// R2: the outcome read order is the resolution file, then the history
// entry; a missing directory recreates nothing and falls through to the
// history; no record of either is not done.
func TestLinkDialogOutcomeReadOrder(t *testing.T) {
	src, _ := linkDialogFixture(t, "pi:chat", []string{})
	id := dialog.NewID()

	// Nothing anywhere: not done.
	if _, done := src.Outcome("pi-chat", id); done {
		t.Fatal("an unresolved dialog reported done")
	}
	// The history entry resolves it after the files are gone.
	if err := dialog.AppendHistory(dialog.HistoryEntry{ID: id, Binding: "pi-chat", Outcome: dialog.OutcomeAllowed, By: dialog.ByBuzz, Label: "Allow once", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r, done := src.Outcome("pi-chat", id)
	if !done || r.Outcome != protocol.ResolutionElsewhere || r.Option != "Allow once" {
		t.Fatalf("history outcome = %+v done=%v; want elsewhere/Allow once", r, done)
	}
	// The history read recreated nothing in the binding's dialog directory.
	dir, _ := dialog.DirRead("pi-chat")
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a retired directory came back: %v", err)
	}

	// A resolution file wins over history and names the link's answer.
	dir2, err := dialog.DirFor("pi-chat") // the seam's write path
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.Resolve(dir2, dialog.Resolved{ID: id, Outcome: dialog.OutcomeAllowed, By: dialog.ByLink, Label: "Allow once", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r, done = src.Outcome("pi-chat", id)
	if !done || r.Outcome != protocol.ResolutionAnswered || r.Option != "Allow once" {
		t.Fatalf("resolution outcome = %+v done=%v; want answered/Allow once", r, done)
	}

	// A timeout resolves as run_ended with no option claimed (a fresh
	// dialog: a resolution file is written once).
	timedOut := dialog.NewID()
	if _, err := dialog.Resolve(dir2, dialog.Resolved{ID: timedOut, Outcome: dialog.OutcomeExpired, By: dialog.ByTimeout, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if r, _ := src.Outcome("pi-chat", timedOut); r.Outcome != protocol.ResolutionRunEnded || r.Option != "" {
		t.Fatalf("timeout outcome = %+v; want run_ended with no option", r)
	}
}

// A resolution by the link's answer maps to answered; by Buzz or the
// terminal, elsewhere.
func TestLinkDialogOutcomeAttribution(t *testing.T) {
	cases := map[string]protocol.ResolutionOutcome{
		dialog.ByLink:     protocol.ResolutionAnswered,
		dialog.ByBuzz:     protocol.ResolutionElsewhere,
		dialog.ByTerminal: protocol.ResolutionElsewhere,
	}
	for by, want := range cases {
		got := mapResolved(dialog.Resolved{ID: "x", Outcome: dialog.OutcomeAllowed, By: by, Label: "L"})
		if got.Outcome != want {
			t.Fatalf("by %q mapped to %s; want %s", by, got.Outcome, want)
		}
	}
}

// An answer file moves the pinned dialog to phase answered; the pinned
// re-render withholds until it resolves, and never swaps in another dialog.
func TestLinkDialogAnsweredPinHolds(t *testing.T) {
	src, rec := linkDialogFixture(t, "pi:chat", []string{})
	id := dialog.NewID()
	openDialog(t, "pi-chat", dialog.Request{ID: id, Harness: "pi", Tool: "Bash", Text: "x", Session: "pi:chat", Options: []dialog.Option{allowOnce}})
	in := src.Select(rec)
	if in == nil || in.InteractionID != id {
		t.Fatalf("selection = %+v; want %s", in, id)
	}
	rec.Interaction = in
	rec.LinkDialogs = []string{id}
	dir, _ := dialog.DirFor("pi-chat")
	if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: id, Option: 1, By: dialog.ByBuzz, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A newer dialog appears too; the pinned dialog must not be replaced by it.
	openDialog(t, "pi-chat", dialog.Request{ID: dialog.NewID(), Harness: "pi", Tool: "Bash", Text: "newer", Session: "pi:chat", Options: []dialog.Option{allowOnce}})
	if got := src.Select(rec); got != nil {
		t.Fatalf("pinned dialog re-render produced %+v; want nil until it resolves", got)
	}
}
