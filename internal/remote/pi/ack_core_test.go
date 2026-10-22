package pi

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestCoreAcknowledgmentReleasesPiResult reproduces Pro deep review finding 6
// (2026-10-22): core acknowledged with the full digest, which includes the
// result's NativeRef, while the adapter compared a NativeRef-stripped digest
// and released nothing.
func TestCoreAcknowledgmentReleasesPiResult(t *testing.T) {
	a, dir := newTestAttachment(t)
	store, err := requests.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return fixedNow },
		Publish: func(protocol.Snapshot, map[string]string) error { return nil }})
	t.Cleanup(func() { _ = ep.Close() })
	ep.Register(a)

	key := testKey("rt")
	key.CreatorHost = "local"
	ref := clientRef(key)
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: key.RequestID, TargetID: key.TargetID, Epoch: addressEpoch("gen-1"),
		NotAfter: "2036-01-01T00:00:00Z", Input: &protocol.SubmitInput{Text: "hi"}}
	if _, err := ep.Handle(cmd, core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	appendEvents(t, dir, ref, fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"completed","text":"answer"}`, ProtocolV1, ref))
	deadline := time.Now().Add(4 * time.Second)
	for {
		_ = ep.Tick()
		if rec, _, _ := store.Get(key); rec != nil && rec.Acknowledged {
			if rec.Result == nil || rec.Result.NativeRef == "" {
				t.Fatalf("want a result with a NativeRef, got %+v", rec.Result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("core never acknowledged the result")
		}
		time.Sleep(time.Millisecond)
	}
	a.mu.Lock()
	r := a.runs[key]
	released := r != nil && r.acked
	a.mu.Unlock()
	if !released {
		t.Fatal("core acknowledged the result but the adapter did not release it")
	}
}
