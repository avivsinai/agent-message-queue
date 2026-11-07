package acp

import (
	"errors"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
	"github.com/avivsinai/agent-message-queue/internal/wakelock"
)

// reattachWindow is how long after its RetiredAt a channel record is still a
// candidate for reattach.
const reattachWindow = 7 * 24 * time.Hour

// The records that prove who runs a session; tests replace them.
var (
	piLiveBridge      = pi.LiveBridge
	reattachWakeOwner = liveWakeOwner
)

// liveWakeOwner is the process the wake lock of root and handle records as
// its owner, when that process still runs with the recorded start and boot.
func liveWakeOwner(root, handle string) (launch.HolderIdentity, bool) {
	owner, ok := wakelock.ReadOwner(root, handle)
	if !ok {
		return launch.HolderIdentity{}, false
	}
	if state, _ := launch.InspectHolder(owner); state != launch.LeaseValid {
		return launch.HolderIdentity{}, false
	}
	return owner, true
}

// reattach restores the share of a session that ended and came back in a new
// process under the same session id. For pi, the pi bridge of the binding's
// root and handle names the session id the ended binding recorded; that
// bridge lives in the handle's own directory under the root and publishes the
// pid of the pi process that runs as that handle, so it proves the process,
// the handle and the session at once. For a Claude mailbox seat, the live
// wake owner of the root and handle is the process, and its own Claude
// session registry entry, written after it started, names the recorded
// session and the same transcript. The owner's detach is never undone
// (binding.DetachedFor), and the binding is written back only if it is still
// as it was read; provision then restores the channel. A native Claude
// session has no wake owner and is restored by the approval hook on its
// first dialog; Codex publishes no record that proves a resume.
//
// The scan runs at most once per channelStepGap. A candidate is a retired
// record of a channel that was active and ended with its session, whose
// RetiredAt is within reattachWindow and before this tick.
func (s *Server) reattach(now time.Time) {
	if !due(s.machine.rebound, channelStepGap, now) {
		return
	}
	stamp := now.UTC().Format(time.RFC3339)
	s.machine.rebound = stamp
	var cands []channelRecord
	for _, r := range s.retiredRecords() {
		at, err := time.Parse(time.RFC3339, r.RetiredAt)
		if r.RetiredBy == retiredEnded && r.RetiredFrom == channelActive && r.ChannelID != "" &&
			err == nil && r.RetiredAt != stamp && now.Sub(at) < reattachWindow {
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
			_, _, _ = binding.Replace(b, nb)
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

// liveAgain returns b held by the process that proves it runs b's recorded
// session: the pi process whose bridge, at b's root and handle, names it, or
// for a Claude mailbox seat the live wake owner of b's root and handle whose
// own registry entry names it with b's transcript.
func liveAgain(b binding.Binding, now time.Time) (binding.Binding, bool) {
	id := b.RecordedSession()
	if id == "" {
		return b, false
	}
	var holder launch.HolderIdentity
	switch {
	case b.Harness == harness.Pi:
		handle := b.Handle
		if !b.Mailbox() {
			handle = strings.TrimPrefix(b.Target, "pi:")
		}
		pid, session, _, live := piLiveBridge(b.Root, handle, now)
		if !live || session != id {
			return b, false
		}
		h, err := launch.HolderOf(pid)
		if err != nil {
			return b, false
		}
		holder = h
	case b.Harness == harness.Claude && b.Mailbox():
		owner, ok := reattachWakeOwner(b.Root, b.Handle)
		if !ok || !claudeRunsSession(b, owner, id) {
			return b, false
		}
		holder = owner
	default:
		return b, false
	}
	nb := b
	nb.Removed, nb.Holder, nb.BoundAt = "", holder, ""
	return nb, true
}

// claudeRunsSession reports whether the Claude process owner runs session id
// with b's transcript: its registry entry, under b's Claude config dir, was
// not written before owner started and names both. An empty transcript proves
// nothing, so it is never a match.
func claudeRunsSession(b binding.Binding, owner launch.HolderIdentity, id string) bool {
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
	return err == nil && s.ID == id && b.Transcript != "" && s.Transcript == b.Transcript
}
