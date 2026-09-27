package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func deliverRenamedMessage(t *testing.T, root, filename, id string) {
	t.Helper()
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "host-bob", To: []string{"alice"},
		Thread: "p2p/alice__host-bob", Subject: "Bridge request",
		Created: "2026-09-22T09:26:05Z", Refs: []string{"earlier-message"}, Kind: format.KindQuestion,
	}, Body: "Check the latest commit."}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxesForTest(t, root, []string{"alice"}, filename, data); err != nil {
		t.Fatal(err)
	}
}

// Regression #848: bridge transfer filenames made native reply reject the ID printed by drain.
func TestReplyByHeaderID(t *testing.T) {
	for _, box := range []string{"new", "cur"} {
		t.Run(box, func(t *testing.T) {
			root := initializedSendMailboxRoot(t, "alice", "host-bob")
			const id = "2026-09-22T10-00-00.000Z_pid12345_a1b2c3d4"
			const filename = "xfer-host-transfer123.md"
			deliverRenamedMessage(t, root, filename, id)
			if box == "cur" {
				if _, _, err := captureEnvOutput(t, func() error {
					return runDrain([]string{"--root", root, "--me", "alice", "--json"})
				}); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := captureEnvOutput(t, func() error {
				return runReply([]string{"--root", root, "--me", "alice", "--id", id, "--body", "Latest commit checked.", "--json"})
			})
			if err != nil {
				t.Fatalf("reply to bridged header ID: %v", err)
			}
			entries, err := os.ReadDir(fsq.AgentInboxNew(root, "host-bob"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("reply delivery: entries=%v err=%v", entries, err)
			}
			reply, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "host-bob"), entries[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			if reply.Header.From != "alice" || !reflect.DeepEqual(reply.Header.To, []string{"host-bob"}) ||
				reply.Header.Thread != "p2p/alice__host-bob" ||
				!reflect.DeepEqual(reply.Header.Refs, []string{"earlier-message", id}) ||
				reply.Header.Kind != format.KindAnswer || reply.Body != "Latest commit checked.\n" {
				t.Fatalf("incorrect bridged reply: %#v", reply)
			}
		})
	}
}

// Regression #848: the same header ID lookup must claim the bridge storage filename.
func TestReadByHeaderIDClaimsStorageFilename(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "host-bob")
	const id = "original-header-id"
	const filename = "xfer-host-read123.md"
	deliverRenamedMessage(t, root, filename, id)
	for i := 0; i < 2; i++ {
		out, _, err := captureEnvOutput(t, func() error {
			return runRead([]string{"--root", root, "--me", "alice", "--id", id, "--json"})
		})
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		var result struct {
			Header format.Header `json:"header"`
			Body   string        `json:"body"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		if result.Header.ID != id || result.Body != "Check the latest commit.\n" {
			t.Fatalf("wrong read result: %#v", result)
		}
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), filename)); !os.IsNotExist(err) {
		t.Fatalf("message still in new: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents/alice/inbox/cur", filename)); err != nil {
		t.Fatalf("storage filename not claimed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "agents/alice/receipts", id+"__alice__drained.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		MessageID string `json:"msg_id"`
		Stage     string `json:"stage"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.MessageID != id || receipt.Stage != "drained" {
		t.Fatalf("incorrect receipt: %s, err=%v", data, err)
	}
}

// PR #849 review F1/F2: unrelated corrupt entries or directories must not
// prevent a valid bridged message from being replied to and claimed by ID.
func TestHeaderIDLookupWithUnrelatedEntries(t *testing.T) {
	for _, scenario := range []struct {
		name string
		box  string
		dir  bool
	}{
		{name: "corrupt-inbox", box: "inbox/new"},
		{name: "corrupt-sent", box: "outbox/sent"},
		{name: "directory", box: "inbox/new", dir: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := initializedSendMailboxRoot(t, "alice", "host-bob")
			const id = "valid-header-id"
			deliverRenamedMessage(t, root, "xfer-host-valid.md", id)
			unrelated := filepath.Join(root, "agents", "alice", scenario.box, "zzz-unrelated.md")
			var err error
			if scenario.dir {
				err = os.Mkdir(unrelated, 0o700)
			} else {
				err = os.WriteFile(unrelated, []byte("not a message"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = captureEnvOutput(t, func() error {
				return runReply([]string{"--root", root, "--me", "alice", "--id", id, "--body", "Checked.", "--json"})
			})
			if err != nil {
				t.Fatalf("reply blocked by unrelated entry: %v", err)
			}
			out, _, err := captureEnvOutput(t, func() error {
				return runRead([]string{"--root", root, "--me", "alice", "--id", id, "--json"})
			})
			if err != nil {
				t.Fatalf("read blocked by unrelated entry: %v", err)
			}
			var result struct {
				Header format.Header `json:"header"`
				Body   string        `json:"body"`
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if result.Header.ID != id || result.Body != "Check the latest commit.\n" {
				t.Fatalf("wrong message read: %#v", result)
			}
			if _, err := os.Stat(unrelated); err != nil {
				t.Fatalf("lookup changed unrelated entry: %v", err)
			}
		})
	}
}

// PR #849 review F3: replay the ReadDir -> drain -> open ordering without a
// timing-dependent goroutine. The stale entry must not stop the cur lookup.
func TestLookupHeaderIDAfterDrainMovesEntry(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "host-bob")
	const id = "moved-header-id"
	const filename = "xfer-host-moved.md"
	deliverRenamedMessage(t, root, filename, id)
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	newDir := filepath.Join("agents", "alice", "inbox", "new")
	entries, err := deliveryRoot.ReadDir(newDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot inbox: entries=%v err=%v", entries, err)
	}
	if err := claimInboxNewToCur(deliveryRoot, "alice", filename); err != nil {
		t.Fatal(err)
	}
	headerID, err := readLookupHeaderID(deliveryRoot, filepath.Join(newDir, entries[0].Name()))
	if err != nil || headerID != "" {
		t.Fatalf("stale entry must be skipped: ID=%q err=%v", headerID, err)
	}
	path, box, err := findMessageDeliveryRoot(deliveryRoot, "alice", id+".md", false)
	if err != nil || box != fsq.BoxCur || path != filepath.Join("agents", "alice", "inbox", "cur", filename) {
		t.Fatalf("moved message lookup: path=%q box=%q err=%v", path, box, err)
	}
}
