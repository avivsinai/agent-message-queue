package buzzio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Regressions for codex's third review of PR #866.

// r3 #1: a refusal the core never stored, already shown to the owner, was
// resubmitted on redelivery once core get returned not_found.
func TestShownRefusalIsNotResubmitted(t *testing.T) {
	r := newRig(t)
	r.rt.WithEvidence(&protocol.Evidence{Submit: "submitted", Completion: "run_terminal"})
	dm := ownerEvent(t, r.owner, r.b.Channel, "execute this", r.now)
	stop := errors.New("interrupted after rejection row receipt was written, before settlement")
	var restore func()
	r.c.handle = func(cmd *protocol.Command, src core.Source) (any, error) {
		out, err := r.ep.Handle(cmd, src)
		if cmd.Op == protocol.OpRequestSubmit {
			reply, ok := out.(protocol.Reply)
			if err != nil || !ok || reply.Snapshot.Code != protocol.CodeUnsupported {
				t.Fatalf("expected unsupported reply, got %+v %v", out, err)
			}
			syncs := 0
			restore = fsq.SyncDirAmbientSwapForTest(func(dir string) error {
				// Production PutReceipt writes row map then ref receipt, each with a
				// directory sync before and after rename. Stop at its final sync.
				if dir == filepath.Join(r.l.dir, "receipts") {
					syncs++
					if syncs == 4 {
						panic(stop)
					}
				}
				f, err := os.Open(dir)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				return f.Sync()
			})
		}
		return out, err
	}
	func() {
		defer func() {
			if restore != nil {
				restore()
			}
			if got := recover(); got != stop {
				t.Fatalf("interruption=%v, want boundary after prepared row", got)
			}
		}()
		_ = r.c.Ingest(dm)
	}()
	r.c.handle = r.ep.Handle
	if _, settled, err := r.l.Settled(dm.ID.Hex()); err != nil || settled {
		t.Fatalf("unexpected settlement: %v %v", settled, err)
	}
	sent := 0
	if err := r.c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error {
		if !strings.Contains(evt.Content, "rejected") || !strings.Contains(evt.Content, "unsupported") {
			t.Fatalf("expected stored rejection row, got %q", evt.Content)
		}
		sent++
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("expected one stored refusal, sent=%d", sent)
	}
	r.rt.WithEvidence(&protocol.Evidence{Submit: "admitted", Completion: "run_terminal"})
	r.now = r.now.Add(2 * time.Second)
	if err := r.c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	if n := r.rt.Snapshot().Dispatches; n != 0 {
		t.Fatalf("same signed event dispatched %d time(s) after its unsupported refusal row was already published", n)
	}
}

// r3 #2: after the approved native session changed, a prior receipt was
// adopted and its update sent under the new binding.
func TestNativeRebindDoesNotAdoptPriorReceipt(t *testing.T) {
	r := newRig(t)
	dm := ownerEvent(t, r.owner, r.b.Channel, "old native work", r.now)
	if err := r.c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	ref := protocol.EncodeRef(r.c.sourceFor(dm).Host, r.b.Target, requestIDFor(r.b, dm.ID.Hex()))
	moved := r.b
	moved.NativeSession = "replacement"
	c2 := r.carrier(t, moved)
	c2.identity = func(string) string { return "replacement" }
	r.now = r.now.Add(2 * time.Second)
	if err := c2.Publish(protocol.Snapshot{RequestRef: ref, Revision: 999, State: protocol.StateCompleted}, r.c.sourceFor(dm).Origin); err != nil {
		return
	}
	sent := 0
	if err := c2.Flush(context.Background(), func(context.Context, nostr.Event) error { sent++; return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("sent %d updates from prior native session under replacement sharing binding", sent)
	}
}
