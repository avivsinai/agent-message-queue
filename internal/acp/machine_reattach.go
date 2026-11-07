package acp

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
)

// The live-session records each harness owns; tests replace them.
var (
	claudeLiveSessions = claude.LiveSessions
	piLiveBridge       = pi.LiveBridge
	reattachHolderOf   = launch.HolderOf
)

// reattach restores the share of a session that ended and came back: a
// running process's own harness record (Claude's session registry, a pi
// bridge's liveness) names the session id the ended binding recorded, the
// identity is the same, and the owner never detached that identity. It
// rebinds that process through binding.WriteSession; provision then
// restores the channel. It restores an earlier share only, never a detach.
// Codex publishes no such record, so a Codex session is never reattached.
//
// The scan runs at most once per channelStepGap. Only a retired record of a
// channel that was active and ended with its session is a candidate, and the
// harness records are read only when one exists.
func (s *Server) reattach(now time.Time) {
	if !due(s.machine.rebound, channelStepGap, now) {
		return
	}
	s.machine.rebound = now.UTC().Format(time.RFC3339)
	var cands []channelRecord
	for _, r := range s.retiredRecords() {
		if r.RetiredBy == retiredEnded && r.RetiredFrom == channelActive && r.ChannelID != "" {
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
	detached := map[string]bool{}
	for _, t := range tombs {
		if t.Removed == binding.RemovedDetached {
			detached[t.SessionIdentity()] = true
		}
	}
	claudes := map[string][]claude.LiveSession{}
	tried := map[string]bool{}
	for _, r := range cands {
		if s.stopped() {
			return
		}
		if tried[r.Name] || detached[r.Identity] {
			continue
		}
		tried[r.Name] = true
		b, ok := endedBinding(r, tombs)
		if !ok {
			continue
		}
		nb, shared, ok := s.liveAgain(b, claudes, now)
		if shared && !r.SaidShared && s.unchanged(r) {
			r.SaidShared = true
			if s.save(&r, now) == nil {
				_ = accepted(s.call("messages", "send", "--channel", r.ChannelID, "--content",
					"Not connected again: two running sessions continue "+r.Label+". Run /amq-remote in the one to connect."))
			}
		}
		if ok {
			_, _, _ = binding.WriteSession(nb, b.Name, binding.Binding.Liveness)
		}
	}
}

// endedBinding is the binding r served, when it is proven ended: a binding
// of r's name and identity that is ended, else that name's tombstone for the
// identity that records an end.
func endedBinding(r channelRecord, tombs []binding.Binding) (binding.Binding, bool) {
	b, err := binding.ReadNamed(r.Name)
	if err == nil {
		state, _ := b.Liveness()
		return b, b.SessionIdentity() == r.Identity && state == binding.Ended
	}
	if !errors.Is(err, binding.ErrNone) {
		return binding.Binding{}, false
	}
	for _, t := range tombs {
		if t.Name == r.Name && t.SessionIdentity() == r.Identity && tombReason(t) == retiredEnded {
			t.Removed = ""
			return t, true
		}
	}
	return binding.Binding{}, false
}

// liveAgain returns b held by the one running process whose harness record
// names b's recorded session, with the same identity. shared is true when
// two running processes name it: neither is bound. claudes caches the
// Claude registry of each config dir for this scan.
func (s *Server) liveAgain(b binding.Binding, claudes map[string][]claude.LiveSession, now time.Time) (nb binding.Binding, shared, ok bool) {
	id := b.RecordedSession()
	if id == "" {
		return b, false, false
	}
	pid := 0
	nb = b
	switch b.Harness {
	case harness.Claude:
		dir := b.ConfigDir
		if dir == "" {
			var err error
			if dir, err = harness.UserClaudeConfigDir(); err != nil {
				return b, false, false
			}
		}
		live, read := claudes[dir]
		if !read {
			live, _ = claudeLiveSessions(dir)
			claudes[dir] = live
		}
		var match *claude.LiveSession
		for i := range live {
			if live[i].ID != id {
				continue
			}
			if match != nil {
				return b, true, false
			}
			match = &live[i]
		}
		if match == nil {
			return b, false, false
		}
		pid = match.PID
		if !b.Mailbox() {
			// A native identity is the harness, root and working directory;
			// a session resumed elsewhere is another identity.
			if binding.NativeIdentity(harness.Claude, b.Root, match.Cwd) != b.SessionIdentity() {
				return b, false, false
			}
			nb.Target = "claude:" + strconv.Itoa(pid)
		}
		if match.Transcript != "" {
			nb.Transcript = match.Transcript
		}
	case harness.Pi:
		handle := b.Handle
		if !b.Mailbox() {
			handle = strings.TrimPrefix(b.Target, "pi:")
		}
		bridgePID, session, _, live := piLiveBridge(b.Root, handle, now)
		if !live || session != id {
			return b, false, false
		}
		pid = bridgePID
	default:
		return b, false, false
	}
	holder, err := reattachHolderOf(pid)
	if err != nil {
		return b, false, false
	}
	nb.Holder, nb.BoundAt = holder, ""
	return nb, false, true
}
