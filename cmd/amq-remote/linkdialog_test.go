package main

import (
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

	// The remaining dispositions through the same chain, one real hook
	// per row: closing its done channel ends the dialog by the terminal;
	// writing the durable resolution file answers it by the link. Each
	// row publishes the next open dialog and moves the revision on
	// publish and on resolve.
	seen := map[string]bool{reqID: true}
	dispositions := []struct {
		name       string
		resolve    func(dir string, id string, done chan struct{})
		wantKind   protocol.ResolutionOutcome
		wantOption string
	}{
		{name: "terminal", resolve: func(dir string, id string, done chan struct{}) { close(done) },
			wantKind: protocol.ResolutionElsewhere, wantOption: ""},
		{name: "link", resolve: func(dir string, id string, done chan struct{}) {
			if _, err := dialog.Resolve(dir, dialog.Resolved{ID: id, Outcome: dialog.OutcomeAllowed, Label: "Allow once", By: dialog.ByLink, At: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
		}, wantKind: protocol.ResolutionAnswered, wantOption: "Allow once"},
	}
	for _, d := range dispositions {
		var rowOut bytes.Buffer
		rowDone := make(chan struct{})
		rowEnded := make(chan struct{})
		go func() {
			defer close(rowEnded)
			claude.RunPermissionHook(bytes.NewReader(stdin), &rowOut, rowDone, nil)
		}()
		var rowID string
		for deadline := time.Now().Add(5 * time.Second); rowID == ""; {
			dir, err := dialog.DirRead("pi-chat")
			if err == nil {
				if reqs, _ := dialog.Requests(dir); len(reqs) > 0 {
					for _, rq := range reqs {
						if !seen[rq.ID] && rq.Complete {
							rowID = rq.ID
						}
					}
				}
			}
			if rowID != "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the hook wrote no dialog for the %s row", d.name)
			}
			time.Sleep(5 * time.Millisecond)
		}
		seen[rowID] = true
		if err := ep.Reconcile(); err != nil {
			t.Fatal(err)
		}
		rep, err := ep.Handle(get, linkSrc)
		if err != nil {
			t.Fatalf("get after publication %s: %v", d.name, err)
		}
		snap = rep.(protocol.Reply).Snapshot
		if snap.Interaction == nil || snap.Interaction.InteractionID != rowID {
			t.Fatalf("snapshot interaction = %+v; want the %s dialog", snap.Interaction, d.name)
		}
		revRow := snap.Revision
		dir, err := dialog.DirFor("pi-chat")
		if err != nil {
			t.Fatal(err)
		}
		d.resolve(dir, rowID, rowDone)
		select {
		case <-rowEnded:
		case <-time.After(5 * time.Second):
			t.Fatalf("the hook did not end after the %s disposition", d.name)
		}
		if err := ep.Reconcile(); err != nil {
			t.Fatal(err)
		}
		rep, err = ep.Handle(get, linkSrc)
		if err != nil {
			t.Fatalf("get after %s resolution: %v", d.name, err)
		}
		snap = rep.(protocol.Reply).Snapshot
		if snap.Interaction != nil {
			t.Fatalf("snapshot kept the %s-resolved interaction: %+v", d.name, snap.Interaction)
		}
		var got2 *protocol.Resolution
		for j := range snap.Resolved {
			if snap.Resolved[j].InteractionID == rowID {
				got2 = &snap.Resolved[j]
			}
		}
		if got2 == nil || got2.Outcome != d.wantKind || got2.Option != d.wantOption {
			t.Fatalf("%s resolution = %+v; want %s/%q", d.name, got2, d.wantKind, d.wantOption)
		}
		if snap.Revision <= revRow {
			t.Fatalf("revision %d after the %s resolution; want above %d", snap.Revision, d.name, revRow)
		}
	}
}
