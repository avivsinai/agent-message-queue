package core_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestAsyncStorageFailureIsVisibleToTheOwner reproduces agent-message-queue
// -611.50: when the durable write of a completion failed, the endpoint built
// an uncertain projection that only test observers saw, so request.get and
// Wait kept showing running until Reconcile. The other rows are the review
// findings on the first fix: the projection must end with the Reconcile pass
// even when that pass writes nothing, and a durable terminal state wins.
func TestAsyncStorageFailureIsVisibleToTheOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	readOnly := func(t *testing.T, store *requests.Store) func() {
		dir := filepath.Join(store.Dir(), "requests", "local")
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		restore := func() { _ = os.Chmod(dir, 0o700) }
		t.Cleanup(restore)
		return restore
	}
	get := func(t *testing.T, ep *core.Endpoint, id string) protocol.Snapshot {
		t.Helper()
		cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: protocol.EncodeRef("local", "fake", id)}
		reply, err := ep.Handle(cmd, core.Source{Host: "local"})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return reply.(protocol.Reply).Snapshot
	}

	t.Run("completion write fails", func(t *testing.T) {
		ep, rt, store, _ := b14cEndpoint(t)
		const id = "11111111-1111-4111-8111-111111111150"
		if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
			t.Fatalf("submit: %v", err)
		}
		restore := readOnly(t, store)
		if !rt.Complete(id, "done") {
			t.Fatal("no running run to complete")
		}
		if s := get(t, ep, id); s.State != protocol.StateUncertain || s.Code != protocol.CodeStorageFull {
			t.Fatalf("get = %s/%s, want uncertain/storage_full", s.State, s.Code)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if s, err := ep.Wait(ctx, protocol.EncodeRef("local", "fake", id)); err != nil || s.State != protocol.StateUncertain {
			t.Fatalf("wait = %s, %v; want uncertain", s.State, err)
		}
		restore()
		if err := ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if s := get(t, ep, id); s.State != protocol.StateCompleted {
			t.Fatalf("get after reconcile = %s, want completed", s.State)
		}
	})

	t.Run("reconcile that writes nothing ends it", func(t *testing.T) {
		ep, rt, store, _ := b14cEndpoint(t)
		const id = "11111111-1111-4111-8111-111111111151"
		if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
			t.Fatalf("submit: %v", err)
		}
		restore := readOnly(t, store)
		rt.LocalInput("typed at the terminal")
		if s := get(t, ep, id); s.State != protocol.StateUncertain {
			t.Fatalf("get = %s, want uncertain", s.State)
		}
		restore()
		if err := ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if s := get(t, ep, id); s.State != protocol.StateRunning {
			t.Fatalf("get after reconcile = %s, want running", s.State)
		}
	})

	t.Run("durable terminal state wins", func(t *testing.T) {
		ep, rt, store, _ := b14cEndpoint(t)
		const id = "11111111-1111-4111-8111-111111111152"
		rt.HoldAdmission()
		t.Cleanup(rt.ReleaseAdmission)
		done := make(chan error, 1)
		go func() {
			_, err := ep.Handle(submitCmd(id), core.Source{Host: "local"})
			done <- err
		}()
		key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		state := func() protocol.State {
			rec, ok, err := store.Get(key)
			if err != nil || !ok {
				return ""
			}
			return rec.State
		}
		if !b14cWait(func() bool { return state() == protocol.StateDispatching }) {
			t.Fatal("submit never reached dispatching")
		}
		rt.CancelRun(id)
		if !b14cWait(func() bool { return state() == protocol.StateCancelled }) {
			t.Fatal("native cancel never reached the record")
		}
		readOnly(t, store)
		rt.ReleaseAdmission()
		<-done
		if s := get(t, ep, id); s.State != protocol.StateCancelled {
			t.Fatalf("get = %s, want the durable cancelled", s.State)
		}
	})
}
