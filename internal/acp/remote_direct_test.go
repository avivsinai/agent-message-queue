package acp

import (
	"encoding/json"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// directNative is a binding named name of the native session "fake" behind
// the endpoint under root (remoteServer).
func directNative(root, name string) binding.Binding {
	return binding.Binding{Root: root, Target: "fake", NativeSession: "fake", Name: name}
}

// directTiming sets the turn durations of f's server. Call it before the
// machine workers start: they read the configuration.
func directTiming(f *standaloneFixture, turnTimeout time.Duration) {
	f.s.cfg.TurnTimeout, f.s.cfg.PollInterval, f.s.cfg.HeartbeatInterval = turnTimeout, 5*time.Millisecond, 10*time.Millisecond
}

// directComplete completes the run of requestID with text once rt has
// admitted it. On the direct path a turn has no ACP client whose updates
// could complete it.
func directComplete(t *testing.T, rt *fake.Runtime, requestID, text string) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		for !rt.Complete(requestID, text) {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
}

// directBusy reports whether the endpoint in dir holds the request ref as a
// busy refusal that a later submit retries.
func directBusy(dir, ref string) bool {
	resp, err := ipc.Call(dir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}, NativeSession: "fake"})
	if err != nil || resp.AsError() != nil {
		return false
	}
	var rep protocol.Reply
	return json.Unmarshal(resp.Reply, &rep) == nil && rep.Snapshot.State == protocol.StateRejected && rep.Snapshot.Code == protocol.CodeBusy
}

// directContents are the contents of evts in order.
func directContents(evts []nostr.Event) []string {
	out := make([]string, 0, len(evts))
	for _, evt := range evts {
		out = append(out, evt.Content)
	}
	return out
}
