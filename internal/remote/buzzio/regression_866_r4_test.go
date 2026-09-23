package buzzio

import (
	"context"
	"errors"
	"fiatjaf.com/nostr"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codex #866 r4: the root row was committed before its id reached the
// receipt; after an interruption between the two, redelivery found no
// decision and resubmitted a refusal already shown to the owner.
func TestPreparedRootBeforeReceiptStopsResubmit(t *testing.T) {
	r := newRig(t)
	r.rt.WithEvidence(&protocol.Evidence{Submit: "submitted", Completion: "run_terminal"})
	dm := ownerEvent(t, r.owner, r.b.Channel, "execute this", r.now)
	stop := errors.New("interrupted before first root receipt rename")
	var restore func()
	r.c.handle = func(cmd *protocol.Command, src core.Source) (any, error) {
		out, err := r.ep.Handle(cmd, src)
		if cmd.Op == protocol.OpRequestSubmit {
			reply, ok := out.(protocol.Reply)
			if err != nil || !ok || reply.Snapshot.Code != protocol.CodeUnsupported {
				t.Fatalf("reply=%+v err=%v", out, err)
			}
			restore = fsq.SyncDirAmbientSwapForTest(func(dir string) error {
				if dir == filepath.Join(r.l.dir, "receipts") {
					panic(stop)
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
				t.Fatalf("interruption=%v", got)
			}
		}()
		_ = r.c.Ingest(dm)
	}()
	r.c.handle = r.ep.Handle
	ref := protocol.EncodeRef(r.c.sourceFor(dm).Host, r.b.Target, requestIDFor(r.b, dm.ID.Hex()))
	rc, found, err := r.l.ReceiptFor(ref)
	if err != nil || !found || rc.RootEventID != "" {
		t.Fatalf("receipt=%+v found=%v err=%v", rc, found, err)
	}
	if _, found, err := r.l.Prepared(rootKey(ref)); err != nil || !found {
		t.Fatalf("durable root=%v err=%v", found, err)
	}
	if _, settled, err := r.l.Settled(dm.ID.Hex()); err != nil || settled {
		t.Fatalf("settled=%v err=%v", settled, err)
	}
	sent := 0
	if err := r.c.Flush(context.Background(), func(_ context.Context, e nostr.Event) error {
		if !strings.Contains(e.Content, "rejected") || !strings.Contains(e.Content, "unsupported") {
			t.Fatalf("unexpected row=%q", e.Content)
		}
		sent++
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("sent=%d", sent)
	}
	r.rt.WithEvidence(&protocol.Evidence{Submit: "admitted", Completion: "run_terminal"})
	r.now = r.now.Add(2 * time.Second)
	if err := r.c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	if n := r.rt.Snapshot().Dispatches; n != 0 {
		t.Fatalf("same signed event dispatched %d time(s) after durable root rejection was shown", n)
	}
}
