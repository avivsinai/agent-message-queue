package pi

// Tests for bridge revision 4 tool approval (protocol: bridge revision 4).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	rec := fmt.Sprintf(`{"protocol":%q,"live":true,"at":%q,"pid":%d,"surface":"rpc","session_generation":"gen-1","bridge_revision":%d}`,
		ProtocolV1, fixedNow.UTC().Format(time.RFC3339Nano), os.Getpid(), revision)
	path := filepath.Join(dir, "bridge.liveness")
	if err := os.WriteFile(path, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fixedNow, fixedNow); err != nil {
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
	return fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"interaction","at":"2023-11-14T22:13:20Z","interaction_id":"tool-1","kind":"approval","prompt":"run: go test ./...","options":["Allow once","Block"],"approve_option":"Allow once","reject_option":"Block","manifest_hash":"sha256:abc","expires_at":"2023-11-14T22:18:20Z","presence":"remote"}`,
		ProtocolV1, ref)
}

func resolvedLine(ref, outcome, option string) string {
	return fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"interaction_resolved","at":"2023-11-14T22:13:30Z","interaction_id":"tool-1","outcome":%q,"option":%q}`,
		ProtocolV1, ref, outcome, option)
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
	path := filepath.Join(dir, "answers", ref+".tool-1.json")
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
