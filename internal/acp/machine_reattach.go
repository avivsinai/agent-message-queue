package acp

import (
	"errors"
	"strings"
	"time"

	amqcli "github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
)

// reattachWindow is how long after its retirement a channel record is still
// a candidate for reattach.
const reattachWindow = 7 * 24 * time.Hour

// The records that prove who runs a session; tests replace them.
var (
	reattachWakeOwner = liveWakeOwner
	piLiveBridge      = pi.LiveBridge
)

// liveWakeOwner is the process the wake lock of root and handle records as
// its owner, when that process still runs with the recorded identity.
func liveWakeOwner(root, handle string) (launch.HolderIdentity, bool) {
	owner, ok := amqcli.WakeOwner(root, handle)
	if !ok {
		return launch.HolderIdentity{}, false
	}
	cur, err := launch.HolderOf(owner.PID)
	if err != nil || cur.ProcessStart != owner.ProcessStart || cur.BootID != owner.BootID {
		return launch.HolderIdentity{}, false
	}
	return cur, true
}

// reattach restores the share of a session that ended and came back in a
// new process under the same session id: a mailbox session (Claude or pi)
// whose new process is the live wake owner of its root and handle and whose
// harness record names the recorded session, or a pi native session whose
// bridge names it. The owner's detach is never undone (binding.DetachedFor).
// The binding is written back only if it is still as it was read; provision
// then restores the channel. A native Claude session is restored by the
// approval hook on its first dialog instead, and Codex publishes no record
// to prove a resume.
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

// liveAgain returns b held by the running process that proves it runs b's
// recorded session, as b's identity.
func liveAgain(b binding.Binding, now time.Time) (binding.Binding, bool) {
	id := b.RecordedSession()
	if id == "" {
		return b, false
	}
	var holder launch.HolderIdentity
	switch {
	case b.Mailbox():
		owner, ok := reattachWakeOwner(b.Root, b.Handle)
		if !ok || !runsSession(b, owner, id, now) {
			return b, false
		}
		holder = owner
	case b.Harness == harness.Pi:
		pid, session, _, live := piLiveBridge(b.Root, strings.TrimPrefix(b.Target, "pi:"), now)
		if !live || session != id {
			return b, false
		}
		h, err := launch.HolderOf(pid)
		if err != nil {
			return b, false
		}
		holder = h
	default:
		return b, false
	}
	nb := b
	nb.Removed, nb.Holder, nb.BoundAt = "", holder, ""
	return nb, true
}

// runsSession reports whether the wake owner of a mailbox binding runs b's
// recorded session id: the Claude process whose own registry entry names
// it with b's transcript, or the pi process whose bridge publishes it.
func runsSession(b binding.Binding, owner launch.HolderIdentity, id string, now time.Time) bool {
	switch b.Harness {
	case harness.Claude:
		started, err := launch.ProcessStartUnix(owner)
		if err != nil {
			return false
		}
		dir := b.ConfigDir
		if dir == "" {
			if dir, err = harness.UserClaudeConfigDir(); err != nil {
				return false
			}
		}
		s, err := claude.SessionOfProcess(dir, owner.PID, time.Unix(started, 0))
		return err == nil && s.ID == id && s.Transcript == b.Transcript
	case harness.Pi:
		pid, session, _, live := piLiveBridge(b.Root, b.Handle, now)
		return live && pid == owner.PID && session == id
	}
	return false
}
