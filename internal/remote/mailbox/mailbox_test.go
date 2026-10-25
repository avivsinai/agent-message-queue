package mailbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// newTestAttachment builds a mailbox target for handle squad whose clock is
// *clk.
func newTestAttachment(t *testing.T, clk *time.Time) (*Attachment, string) {
	t.Helper()
	root := t.TempDir()
	for _, box := range []string{"new", "cur", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, "agents", "squad", "inbox", box), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	att, err := Factory(context.Background(), registry.FactoryConfig{Root: root, Target: "squad", Me: "remote", Config: json.RawMessage(`{"handle":"squad"}`)})
	if err != nil {
		t.Fatal(err)
	}
	a := att.(*Attachment)
	a.now = func() time.Time { return *clk }
	return a, root
}

func faultSyncs(t *testing.T, fn func(dir string) error) {
	t.Helper()
	fsq.SetPackageSyncDirFaultForTest(fn)
	t.Cleanup(func() { fsq.SetPackageSyncDirFaultForTest(nil) })
}

func inboxCount(t *testing.T, root string) int {
	t.Helper()
	names, err := os.ReadDir(filepath.Join(root, "agents", "squad", "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

var testKey = requests.Key{CreatorHost: "local", TargetID: "squad", RequestID: "11111111-1111-4111-8111-111111111111"}

func submitReq(clk time.Time) core.BoundRequest {
	return core.BoundRequest{Key: testKey, Epoch: "mailbox:squad", Input: protocol.SubmitInput{Text: "fix the build"}, NotAfter: protocol.FormatTime(clk.Add(time.Minute))}
}

// Regression (PR #1226 review): not_after was checked only before the
// journal entry and its barrier, so a slow barrier delivered the message
// after the window closed.
func TestSubmitDeliversNothingWhenTheWindowClosesDuringTheJournalBarrier(t *testing.T) {
	clk := time.Now().UTC()
	a, root := newTestAttachment(t, &clk)
	req := submitReq(clk)
	faultSyncs(t, func(dir string) error {
		if strings.HasPrefix(filepath.ToSlash(dir), "extensions/remote/mailbox") {
			clk = clk.Add(2 * time.Minute)
		}
		return nil
	})
	adm, err := a.Submit(req)
	if err != nil || adm.Admitted || adm.Code != protocol.CodeExpired {
		t.Fatalf("Submit = %+v, %v; want expired", adm, err)
	}
	if n := inboxCount(t, root); n != 0 {
		t.Fatalf("handle inbox holds %d messages after the window closed", n)
	}
	if err := a.Forget(testKey); err != nil {
		t.Fatalf("Forget after a late window: %v", err)
	}
}

// Regression (PR #1226 review): the checked ack swallowed a reply move
// whose inbox directory sync failed and recorded Acked anyway.
func TestCheckedAckFailsUntilTheReplyMoveIsDurable(t *testing.T) {
	clk := time.Now().UTC()
	a, root := newTestAttachment(t, &clk)
	if adm, err := a.Submit(submitReq(clk)); err != nil || !adm.Admitted {
		t.Fatalf("Submit = %+v, %v", adm, err)
	}
	names, _ := os.ReadDir(filepath.Join(root, "agents", "squad", "inbox", "new"))
	prompt, err := format.ReadMessageFile(filepath.Join(root, "agents", "squad", "inbox", "new", names[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	replyID, _ := format.NewMessageID(time.Now())
	reply := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: replyID, From: "squad", To: []string{prompt.Header.From}, Thread: prompt.Header.Thread,
		Refs: []string{prompt.Header.ID}, Created: time.Now().UTC().Format(time.RFC3339Nano), Kind: string(format.KindAnswer),
	}, Body: "done"}
	data, _ := reply.Marshal()
	if err := os.WriteFile(filepath.Join(root, "agents", prompt.Header.From, "inbox", "new", replyID+".md"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ev, err := a.Lookup(testKey, "")
	if err != nil || ev.State != protocol.StateCompleted {
		t.Fatalf("Lookup = %+v, %v; want the completed reply", ev, err)
	}
	digest := protocol.EvidenceDigest(ev.Result)

	failing := true
	faultSyncs(t, func(dir string) error {
		if failing && strings.HasSuffix(filepath.ToSlash(dir), "inbox/cur") {
			return errors.New("injected inbox sync failure")
		}
		return nil
	})
	if err := a.AcknowledgeResultChecked(testKey, "", digest); err == nil {
		t.Fatal("checked ack succeeded while the reply move was not durable")
	}
	if ev, err := a.Lookup(testKey, ""); err != nil || ev.State != protocol.StateCompleted {
		t.Fatalf("Lookup after the failed ack = %+v, %v; want the result still held", ev, err)
	}
	failing = false
	if err := a.AcknowledgeResultChecked(testKey, "", digest); err != nil {
		t.Fatalf("checked ack retry: %v", err)
	}
	if ev, err := a.Lookup(testKey, ""); err != nil || ev.Class != core.EvidenceNone {
		t.Fatalf("Lookup after the ack = %+v, %v; want none", ev, err)
	}
}

// Regression (PR #1226 review): Submit accepted a journal state whose
// barrier had failed, so a Lookup tombstone that was visible but not durable
// became a settled refusal.
func TestSubmitMakesAVisibleTombstoneDurableBeforeRefusing(t *testing.T) {
	clk := time.Now().UTC()
	a, root := newTestAttachment(t, &clk)
	failing := true
	faultSyncs(t, func(dir string) error {
		if failing && strings.HasPrefix(filepath.ToSlash(dir), "extensions/remote/mailbox") {
			return errors.New("injected journal sync failure")
		}
		return nil
	})
	if _, err := a.Lookup(testKey, ""); err == nil {
		t.Fatal("Lookup reported a tombstone whose barrier failed")
	}
	if adm, err := a.Submit(submitReq(clk)); err == nil {
		t.Fatalf("Submit = %+v with the tombstone not durable; want the barrier error", adm)
	}
	failing = false
	if adm, err := a.Submit(submitReq(clk)); err != nil || adm.Admitted || adm.Code != protocol.CodeNativeError {
		t.Fatalf("Submit = %+v, %v; want the not-delivered refusal", adm, err)
	}
	if n := inboxCount(t, root); n != 0 {
		t.Fatalf("handle inbox holds %d messages", n)
	}
}
