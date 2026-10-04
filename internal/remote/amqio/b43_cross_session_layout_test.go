package amqio

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestReplyToMissingPeerMailboxIsRefusedNotCreated covers both arms of
// destination() that route a reply into another root: cross-project (Pro B4,
// reply_project set) and cross-session (agent-message-queue-611.22.43,
// reply_to carries a session). DeliverToInboxes creates a missing mailbox, so
// without ValidateExistingMailboxLayout a reply to a peer root whose mailbox
// does not exist made a black-hole mailbox in someone else's root. A missing
// peer mailbox is transient (it may be provisioned a moment later): the
// command stays in new, is not DLQ'd, and nothing is written into the peer.
func TestReplyToMissingPeerMailboxIsRefusedNotCreated(t *testing.T) {
	cases := []struct {
		name                      string
		replyProject, replyTo     string
		wantProject, wantRoutedTo string
	}{
		{name: "cross-project", replyProject: "peer", replyTo: "codex@session1", wantProject: "peer", wantRoutedTo: "codex@session1"},
		{name: "cross-session", replyTo: "codex@session1", wantRoutedTo: "codex@session1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpointRoot := t.TempDir()
			peerRoot := t.TempDir() // a real root, but the codex mailbox is NOT provisioned
			for _, root := range []string{endpointRoot, peerRoot} {
				if err := fsq.EnsureRootDirs(root); err != nil {
					t.Fatal(err)
				}
			}
			if err := fsq.EnsureAgentDirs(endpointRoot, DefaultHandle); err != nil {
				t.Fatal(err)
			}
			store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			var carrier *Carrier
			ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
				return carrier.Publish(s, origin)
			}})
			carrier, err = New(endpointRoot, DefaultHandle, ep)
			if err != nil {
				t.Fatalf("carrier: %v", err)
			}
			var routedProject, routedReplyTo string
			carrier.SetReplyRouter(func(replyProject, replyTo string) (string, string, error) {
				routedProject, routedReplyTo = replyProject, replyTo
				return peerRoot, "codex", nil
			})
			ep.Register(fake.New("fake", "e_1"))
			t.Cleanup(func() { _ = ep.Close() })

			body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111431","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
			id := deliverCommand(t, endpointRoot, "codex", body, func(h *format.Header) {
				h.ReplyProject = tc.replyProject
				h.ReplyTo = tc.replyTo
			})

			n, err := carrier.ImportOnce()
			if err != nil || n != 0 {
				t.Fatalf("ImportOnce = (%d, %v), want (0, nil): a transient route refusal leaves the command in new", n, err)
			}
			if routedProject != tc.wantProject || routedReplyTo != tc.wantRoutedTo {
				t.Fatalf("router called with (%q, %q), want (%q, %q)", routedProject, routedReplyTo, tc.wantProject, tc.wantRoutedTo)
			}
			if _, err := os.Stat(filepath.Join(peerRoot, "agents", "codex")); err == nil {
				t.Fatal("codex mailbox created in the peer root (black hole)")
			}
			if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(endpointRoot, DefaultHandle), id+".md")); err != nil {
				t.Fatalf("command left new after a transient route refusal: %v", err)
			}
			if n := countFiles(t, filepath.Join(endpointRoot, "agents", DefaultHandle, "dlq", "new")); n != 0 {
				t.Fatalf("transient refusal was DLQ'd: %d", n)
			}
		})
	}
}
