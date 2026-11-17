package mailprompt

import (
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

// Reply is one message in a prompt's reply inbox that answers it:
// its file name, the box that held it when found ("new" or "cur"), and its
// header.
type Reply struct {
	Name   string        `json:"name"`
	Box    string        `json:"box"`
	Header format.Header `json:"header"`
}

// FindReplies returns, oldest first, the messages in inbox (new, then cur)
// that handle sent on thread, ref promptID and were created no earlier than
// since: the match rule of a binding's reply watch. Every read goes through
// root, and nothing is moved or written.
func FindReplies(root *fsq.DeliveryRoot, inbox, handle, thread, promptID string, since time.Time) ([]Reply, error) {
	var out []Hit
	boxes := map[string]string{}
	for _, box := range []string{"new", "cur"} {
		hits, err := newRootScan(root, filepath.Join("agents", inbox, "inbox", box)).Replies(handle, thread, promptID, since)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			if _, seen := boxes[h.Filename]; !seen {
				boxes[h.Filename] = box
				out = append(out, h)
			}
		}
	}
	SortHits(out)
	replies := make([]Reply, 0, len(out))
	for _, h := range out {
		replies = append(replies, Reply{Name: h.Filename, Box: boxes[h.Filename], Header: h.Header})
	}
	return replies, nil
}

// ConsumeReply drains one reply found by FindReplies for consumer, the
// owner of inbox: a move from new to cur and the drained receipt. nil means
// both are durable. A move whose directory sync failed is returned; a reply
// already gone from new (an earlier move, maybe unsynced, or another
// consumer's) gets both inbox directories synced again. Both steps are
// idempotent.
func ConsumeReply(root *fsq.DeliveryRoot, consumer string, r Reply) error {
	err := fsq.MoveNewToCur(root, consumer, r.Name)
	if os.IsNotExist(err) {
		inbox := filepath.Join("agents", consumer, "inbox")
		if err = root.SyncDir(filepath.Join(inbox, "cur")); err == nil {
			err = root.SyncDir(filepath.Join(inbox, "new"))
		}
	}
	if err != nil {
		return err
	}
	return drainedReceipt(root, consumer, r.Header)
}

// drainedReceipt records that consumer drained the reply h. It is
// idempotent: one file per message id.
func drainedReceipt(root *fsq.DeliveryRoot, consumer string, h format.Header) error {
	return receipt.EmitDeliveryRoot(root, receipt.New(h.ID, h.Thread, h.From, consumer, receipt.StageDrained, ""))
}
