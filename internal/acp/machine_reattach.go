package acp

import (
	"errors"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
)

// reattachWindow is how long after its retirement a channel record is still
// a candidate for reattach.
const reattachWindow = 7 * 24 * time.Hour

// piLiveBridge reads a pi bridge's liveness; tests replace it.
var piLiveBridge = pi.LiveBridge

// reattach restores the share of a pi session that ended and came back in a
// new process under the same session id: the pi bridge of the binding's root
// and handle names the session id the ended binding recorded. That bridge
// lives in the handle's own directory under the root and publishes the pid of
// the pi process that runs as that handle, so it proves the process, the
// handle and the session at once. The owner's detach is never undone
// (binding.DetachedFor), and the binding is written back only if it is still
// as it was read; provision then restores the channel. A Claude session is
// restored by the approval hook on its first dialog, which finds the Claude
// process above it; Codex publishes no record that proves a resume.
//
// The scan runs at most once per channelStepGap. A candidate is a retired
// record of a channel that was active and ended with its session, retired
// within reattachWindow and before this tick.
func (s *Server) reattach(now time.Time) {
	if !due(s.machine.rebound, channelStepGap, now) {
		return
	}
	stamp := now.UTC().Format(time.RFC3339)
	s.machine.rebound = stamp
	var cands []channelRecord
	for _, r := range s.retiredRecords() {
		at, err := time.Parse(time.RFC3339, r.Updated)
		if r.RetiredBy == retiredEnded && r.RetiredFrom == channelActive && r.ChannelID != "" &&
			err == nil && r.Updated != stamp && now.Sub(at) < reattachWindow {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return
	}
	tombs, err := binding.Tombstones()
	if err != nil {
		return
	}
	tried := map[string]bool{}
	for _, r := range cands {
		if s.stopped() {
			return
		}
		if tried[r.Name] {
			continue
		}
		tried[r.Name] = true
		b, pruned, ok := endedBinding(r, tombs)
		if !ok {
			continue
		}
		nb, ok := liveAgain(b, now)
		if !ok {
			continue
		}
		if _, off := binding.DetachedFor(nb, tombs); off {
			continue
		}
		if pruned {
			_, _ = binding.ReviveEnded(b, nb)
		} else {
			_, _ = binding.Replace(b, nb)
		}
	}
}

// endedBinding is the binding r served, as read, when it is proven ended: a
// binding of r's name and identity that is ended, else that name's tombstone
// of the identity that records an end (pruned).
func endedBinding(r channelRecord, tombs []binding.Binding) (b binding.Binding, pruned, ok bool) {
	b, err := binding.ReadNamed(r.Name)
	if err == nil {
		state, _ := b.Liveness()
		return b, false, b.SessionIdentity() == r.Identity && state == binding.Ended
	}
	if !errors.Is(err, binding.ErrNone) {
		return binding.Binding{}, false, false
	}
	for _, t := range tombs {
		if t.Name == r.Name && t.SessionIdentity() == r.Identity && t.Removed == binding.RemovedEnded {
			return t, true, true
		}
	}
	return binding.Binding{}, false, false
}

// liveAgain returns b held by the pi process whose bridge, at b's root and
// handle, names b's recorded session.
func liveAgain(b binding.Binding, now time.Time) (binding.Binding, bool) {
	id := b.RecordedSession()
	if b.Harness != harness.Pi || id == "" {
		return b, false
	}
	handle := b.Handle
	if !b.Mailbox() {
		handle = strings.TrimPrefix(b.Target, "pi:")
	}
	pid, session, _, live := piLiveBridge(b.Root, handle, now)
	if !live || session != id {
		return b, false
	}
	holder, err := launch.HolderOf(pid)
	if err != nil {
		return b, false
	}
	nb := b
	nb.Removed, nb.Holder, nb.BoundAt = "", holder, ""
	return nb, true
}
