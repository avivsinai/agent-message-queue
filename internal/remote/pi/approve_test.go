package pi

// Tests for bridge revision 4 tool approval (protocol: bridge revision 4).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// stampLivenessRevision writes a fresh gen-1 liveness record at fixedNow
// that advertises the given bridge_revision.
func stampLivenessRevision(t *testing.T, dir string, revision int) {
	t.Helper()
	stampLivenessRevisionAt(t, dir, fixedNow, revision)
}

// stampLivenessRevisionAt writes a gen-1 liveness record fresh at at.
func stampLivenessRevisionAt(t *testing.T, dir string, at time.Time, revision int) {
	t.Helper()
	rec := fmt.Sprintf(`{"protocol":%q,"live":true,"at":%q,"pid":%d,"surface":"rpc","session_generation":"gen-1","bridge_revision":%d,"session_id":"sess-1"}`,
		ProtocolV1, at.UTC().Format(time.RFC3339Nano), os.Getpid(), revision)
	path := filepath.Join(dir, "bridge.liveness")
	if err := os.WriteFile(path, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// approvalRun is a revision-4 bridge with one admitted request, subscribed
// to the attachment's native events.
func approvalRun(t *testing.T) (*Attachment, string, requests.Key, <-chan core.NativeEvent) {
	t.Helper()
	a, dir := newTestAttachment(t)
	stampLivenessRevision(t, dir, ApproveBridgeRevision)
	key := testKey("approve")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "gen-1")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	ch := make(chan core.NativeEvent, 8)
	t.Cleanup(a.Subscribe(func(ev core.NativeEvent) { ch <- ev }))
	return a, dir, key, ch
}

func interactionLine(ref string) string {
	return interactionLineID(ref, "tool-1")
}

// interactionLineID raises approval id, expiring five minutes after fixedNow.
func interactionLineID(ref, id string) string {
	return fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"interaction","at":"2023-11-14T22:13:20Z","interaction_id":%q,"kind":"approval","prompt":"run: go test ./...","options":["Allow once","Block"],"approve_option":"Allow once","reject_option":"Block","manifest_hash":"sha256:abc","expires_at":"2023-11-14T22:18:20Z","presence":"remote"}`,
		ProtocolV1, ref, id)
}

func resolvedLine(ref, outcome, option string) string {
	return fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"interaction_resolved","at":"2023-11-14T22:13:30Z","interaction_id":"tool-1","outcome":%q,"option":%q}`,
		ProtocolV1, ref, outcome, option)
}

func terminalLine(ref, event string) string {
	return fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":%q,"text":"done"}`, ProtocolV1, ref, event)
}

func nextEvent(t *testing.T, ch <-chan core.NativeEvent, want core.NativeEventType) core.NativeEvent {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Type != want {
			t.Fatalf("event = %q, want %q", ev.Type, want)
		}
		return ev
	case <-time.After(time.Second):
		t.Fatalf("no %q event", want)
	}
	return core.NativeEvent{}
}

// TestApprovalAnsweredFromRemote: an interaction line becomes the pending
// approval, Respond writes the bound answer file, and interaction_resolved
// answered publishes a remote resolution.
func TestApprovalAnsweredFromRemote(t *testing.T) {
	a, dir, key, ch := approvalRun(t)
	ref := clientRef(key)
	appendEvents(t, dir, ref, interactionLine(ref))

	ev, err := a.Lookup(key, "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	in := ev.Interaction
	if in == nil || in.InteractionID != "tool-1" || in.Kind != "approval" || !in.RemoteAnswer ||
		in.ApproveOption != "Allow once" || in.RejectOption != "Block" || len(in.Options) != 2 {
		t.Fatalf("interaction = %+v, want tool-1 with approve and reject", in)
	}
	if q := nextEvent(t, ch, core.EventQuestion); q.Key != key || q.Interaction.InteractionID != "tool-1" {
		t.Fatalf("question = %+v", q)
	}
	s := a.Inspect()
	if !s.Capabilities.ApproveTool || s.PendingInteraction == nil || *s.PendingInteraction != "tool-1" {
		t.Fatalf("session = %+v, want approve_tool and pending tool-1", s)
	}

	code, err := a.Respond(key, "gen-1", "tool-1", "Allow once")
	if err != nil || code != "" {
		t.Fatalf("Respond = %q, %v", code, err)
	}
	path := filepath.Join(dir, "answers", answerName(ref, "tool-1"))
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("answer file: %v, %v", st, err)
	}
	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"protocol": ProtocolV1, "ref": ref, "interaction_id": "tool-1", "manifest_hash": "sha256:abc",
		"option": "Allow once", "at": protocol.FormatTime(fixedNow),
	}
	if len(got) != len(want) {
		t.Fatalf("answer = %s, want exactly %v", data, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("answer[%s] = %v, want %v", k, got[k], v)
		}
	}

	appendEvents(t, dir, ref, resolvedLine(ref, "answered", "Allow once"))
	if ev, err := a.Lookup(key, "gen-1"); err != nil || ev.Interaction != nil {
		t.Fatalf("after resolution: interaction = %+v, %v", ev.Interaction, err)
	}
	if r := nextEvent(t, ch, core.EventQuestionResolved); !r.Remote || r.Key != key {
		t.Fatalf("resolved = %+v, want remote", r)
	}
}

// TestApprovalResolvedElsewhere: a local answer clears the approval with
// Remote false, after the question it resolves.
func TestApprovalResolvedElsewhere(t *testing.T) {
	a, dir, key, ch := approvalRun(t)
	ref := clientRef(key)
	appendEvents(t, dir, ref, interactionLine(ref), resolvedLine(ref, "answered_elsewhere", "Block"))
	if _, err := a.Lookup(key, "gen-1"); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, ch, core.EventQuestion)
	if r := nextEvent(t, ch, core.EventQuestionResolved); r.Remote {
		t.Fatalf("resolved = %+v, want Remote false", r)
	}
}

// Pro review of #926, 2026-09-30, #2: an approval raised, or resolved,
// while amq-remote was down was lost, because the native event had no
// subscriber and reconcile took the same-run shortcut; and a terminal
// result closed an approval as answered on the answer intent, over the
// harness's answered_elsewhere. Reconcile now recovers both from Lookup.
func TestApprovalRecoveredAcrossRestart(t *testing.T) {
	dir := newExtDir(t)
	stampLivenessRevision(t, dir, ApproveBridgeRevision)
	store, err := requests.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	defer func() { _ = ep.Close() }()
	ep.Register(mustAttach(t, dir))
	src := core.Source{Host: "host1"}
	id := "00000000-0000-4000-8000-000000000042"
	ref := protocol.EncodeRef(src.Host, "pi-1", id)
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	_, err = ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: id, TargetID: "pi-1",
		Epoch: addressEpoch("gen-1"), NotAfter: protocol.FormatTime(time.Now().Add(time.Hour)),
		Input: &protocol.SubmitInput{Text: "hi", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn, MinEvidence: protocol.EvidenceSubmitted}}, src)
	if err != nil {
		t.Fatal(err)
	}
	get := func() protocol.Snapshot {
		t.Helper()
		out, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}, src)
		if err != nil {
			t.Fatal(err)
		}
		return out.(protocol.Reply).Snapshot
	}
	restart := func(lines ...string) {
		t.Helper()
		ep.UnregisterAll()
		appendEvents(t, dir, ref, lines...)
		ep.Register(mustAttach(t, dir))
		if err := ep.Reconcile(); err != nil {
			t.Fatal(err)
		}
	}

	restart(interactionLine(ref))
	if s := get(); s.State != protocol.StateRunning || s.Interaction == nil || s.Interaction.InteractionID != "tool-1" || !s.Interaction.RemoteAnswer {
		t.Fatalf("after restart: %+v, want running with approval tool-1 pending", s)
	}
	if _, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref, TargetID: "pi-1",
		Epoch: addressEpoch("gen-1"), InteractionID: "tool-1", Option: "Allow once"}, src); err != nil {
		t.Fatal(err)
	}

	restart(resolvedLine(ref, "answered_elsewhere", "Block"), terminalLine(ref, "completed"))
	s := get()
	if s.State != protocol.StateCompleted || s.Interaction != nil || len(s.Resolved) != 1 ||
		s.Resolved[0].InteractionID != "tool-1" || s.Resolved[0].Outcome != protocol.ResolutionElsewhere || s.Resolved[0].Option != "Block" {
		t.Fatalf("after completion: %+v, resolved %+v, want completed with tool-1 answered_elsewhere (Block)", s, s.Resolved)
	}
}

// Pro review of #926, 2026-09-30, #2: a run that went uncertain with an
// approval open published no resolution. It now closes it as run_ended.
func TestApprovalClosedWhenRunGoesUncertain(t *testing.T) {
	a, dir, key, ch := approvalRun(t)
	ref := clientRef(key)
	appendEvents(t, dir, ref, interactionLine(ref), fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"uncertain","error":"shutdown"}`, ProtocolV1, ref))
	ev, err := a.Lookup(key, "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Resolved) != 1 || ev.Resolved[0].Outcome != protocol.ResolutionRunEnded {
		t.Fatalf("resolved = %+v, want tool-1 run_ended", ev.Resolved)
	}
	nextEvent(t, ch, core.EventQuestion)
	if r := nextEvent(t, ch, core.EventQuestionResolved); r.Outcome != protocol.ResolutionRunEnded || r.Remote {
		t.Fatalf("resolved = %+v, want run_ended, not remote", r)
	}
}

// Pro review of #926, 2026-09-30, #3: a retry of an answer published before
// expiry returned expired after it, a false refusal that dropped the
// endpoint's answer intent. An exact answer on disk is delivered first.
func TestApprovalAnswerReplayAfterExpiry(t *testing.T) {
	a, dir, key, _ := approvalRun(t)
	ref := clientRef(key)
	appendEvents(t, dir, ref, interactionLine(ref))
	if code, err := a.Respond(key, "gen-1", "tool-1", "Allow once"); err != nil || code != "" {
		t.Fatalf("Respond = %q, %v", code, err)
	}
	later := mustAttach(t, dir)
	later.now = func() time.Time { return fixedNow.Add(10 * time.Minute) }
	if code, err := later.Respond(key, "gen-1", "tool-1", "Allow once"); err != nil || code != "" {
		t.Fatalf("replay after expiry = %q, %v, want delivered", code, err)
	}
}

// Pro review of #926, 2026-09-30, #4: an approval offered a remote answer
// while the bridge was revision 3 or the approval had expired, although
// Respond refused both. Only an answer that can apply is offered.
func TestApprovalNotOfferedWhenAnswerCannotApply(t *testing.T) {
	a, dir, key, _ := approvalRun(t)
	ref := clientRef(key)
	appendEvents(t, dir, ref, interactionLine(ref))
	stampLivenessRevision(t, dir, MinBridgeRevision)
	if ev, err := a.Lookup(key, "gen-1"); err != nil || ev.Interaction == nil || ev.Interaction.RemoteAnswer || ev.Interaction.ApproveOption != "" {
		t.Fatalf("revision 3: interaction = %+v, %v, want no remote answer", ev.Interaction, err)
	}
	expired := fixedNow.Add(10 * time.Minute)
	stampLivenessRevisionAt(t, dir, expired, ApproveBridgeRevision)
	a.now = func() time.Time { return expired }
	if ev, err := a.Lookup(key, "gen-1"); err != nil || ev.Interaction == nil || ev.Interaction.RemoteAnswer || ev.Interaction.ApproveOption != "" {
		t.Fatalf("expired: interaction = %+v, %v, want no remote answer", ev.Interaction, err)
	}
}

// Pro review of #926, 2026-09-30, #5: the answer file name was the ref plus
// the interaction id, too long for a long ref and a 128-character id, and
// the same file for ids that differ only in case on a case-insensitive
// filesystem. The name is now a fixed-length hash of both.
func TestApprovalAnswerNameBounded(t *testing.T) {
	a, dir := newTestAttachment(t)
	stampLivenessRevision(t, dir, ApproveBridgeRevision)
	key := requests.Key{CreatorHost: strings.Repeat("h", 100), TargetID: "pi-1", RequestID: "00000000-0000-4000-8000-000000000043"}
	ref := clientRef(key)
	seedRequest(t, dir, ref, "gen-1")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	id := strings.Repeat("A", 128)
	appendEvents(t, dir, ref, interactionLineID(ref, id))
	if code, err := a.Respond(key, "gen-1", id, "Allow once"); err != nil || code != "" {
		t.Fatalf("Respond = %q, %v", code, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "answers", answerName(ref, id))); err != nil {
		t.Fatal(err)
	}
	if answerName(ref, "tool-A") == answerName(ref, "tool-a") {
		t.Fatal("ids that differ in case share an answer file")
	}
}
