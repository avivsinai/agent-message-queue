package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/sender"
)

// spoolSubmit persists a submit envelope for the fake target the way submit
// does, and returns the spool and the command.
func spoolSubmit(t *testing.T, root, id string, notAfter time.Time) (*sender.Spool, *protocol.Command) {
	t.Helper()
	stateDir := filepath.Join(root, "extensions", "remote")
	spool, err := sender.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(notAfter),
		Input:    &protocol.SubmitInput{Text: "say hi", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}
	env := &sender.Envelope{RequestID: id, CreatorHost: ipc.LocalHost, TargetID: "fake", Epoch: "e_1", NotAfter: cmd.NotAfter, Command: cmd, Destination: "ipc:" + stateDir}
	if err := spool.Create(env); err != nil {
		t.Fatal(err)
	}
	return spool, cmd
}

// A same-id retry of an expired envelope keeps the persisted deadline, so the
// endpoint refuses it as expired and status agrees. Regression for
// agent-message-queue-611.69: the retry sent a fresh deadline, ran, and
// status <id> still said expired.
func TestSubmitRetryOfExpiredRequestIsRefused(t *testing.T) {
	root := startServe(t)
	id := "11111111-1111-4111-8111-1111111169a1"
	spool, _ := spoolSubmit(t, root, id, time.Now().Add(-time.Minute))
	if err := spool.MarkExpired(sender.Key{CreatorHost: ipc.LocalHost, RequestID: id}); err != nil {
		t.Fatal(err)
	}

	code, out, _ := cli(t, "", "submit", "fake", "--text", "say hi", "--request-id", id, "--epoch", "e_1", "--root", root, "--json")
	var rep protocol.Reply
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("submit exit=%d out=%s: %v", code, out, err)
	}
	if code == protocol.ExitSuccess || rep.Outcome.Code != protocol.CodeExpired {
		t.Fatalf("retry exit=%d code=%q, want refused as expired: %s", code, rep.Outcome.Code, out)
	}
	scode, sout, _ := cli(t, "", "status", id, "--root", root, "--json")
	if scode != protocol.ExitError || !strings.Contains(sout, `"expired"`) {
		t.Fatalf("status exit=%d out=%s, want expired with exit %d", scode, sout, protocol.ExitError)
	}
}

// status <id> reports the endpoint's record when the endpoint ran the
// request, even though the sender envelope settled as failed. Regression for
// agent-message-queue-611.69: status <id> returned the sender receipt while
// status <ref> said running.
func TestStatusPrefersEndpointRecordOverSenderFailure(t *testing.T) {
	root := startServe(t)
	id := "11111111-1111-4111-8111-1111111169a2"
	spool, cmd := spoolSubmit(t, root, id, time.Now().Add(2*time.Minute))
	if err := spool.MarkFailed(sender.Key{CreatorHost: ipc.LocalHost, RequestID: id}, string(protocol.CodeUnshared)); err != nil {
		t.Fatal(err)
	}
	if rep, err := callReply(filepath.Join(root, "extensions", "remote"), cmd); err != nil || rep.Outcome.Code != "" {
		t.Fatalf("endpoint submit: rep=%+v err=%v", rep, err)
	}

	code, out, _ := cli(t, "", "status", id, "--root", root, "--json")
	var rep protocol.Reply
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status exit=%d out=%s: %v", code, out, err)
	}
	if code != protocol.ExitSuccess || rep.Snapshot.Schema != protocol.SchemaRequest || rep.Snapshot.RequestID != id {
		t.Fatalf("status exit=%d out=%s, want the endpoint record", code, out)
	}
}
