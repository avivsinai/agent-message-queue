package acp

import (
	"fmt"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// A session no binding serves has a session record (package dialog) when
// one of its dialogs waited. The machine agent gives each record whose
// process runs a channel of its own, through the same record path as a
// binding's, under the record's name: the channel shows that session's
// dialogs and answers any other message with approvals only. The record's
// identity is its process, so a record that a later session takes over is
// another channel. When the process is proven gone, the channel is retired
// and the record dropped. When a binding appears that the record's process
// holds (the session ran /amq-remote), the channel record moves to the
// binding's name, so the channel stays the session's, and the session
// record is marked handed: its open dialogs are still posted in that
// channel until they resolve, then its directory is removed.

// sessionIdentity is the channel record identity of a session record.
func sessionIdentity(rec dialog.SessionRecord) string {
	h := rec.Holder
	return fmt.Sprintf("session:%s:%d:%s:%s", rec.Harness, h.PID, h.ProcessStart, h.BootID)
}

// provisionSessions works through every session record: retire and drop
// one whose process is proven gone, hand one over to the binding its
// process now holds (only when listed, the binding list was read), and
// provision the rest. A record whose process cannot be checked is left as
// it is.
func (s *Server) provisionSessions(all []binding.Binding, listed bool, now time.Time) {
	recs, err := dialog.Sessions()
	if err != nil {
		return
	}
	names := make([]string, 0, len(recs))
	for name := range recs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if s.stopped() {
			return
		}
		rec := recs[name]
		switch state, _ := launch.InspectHolder(rec.Holder); {
		case state == launch.LeaseStale:
			r, ok, err := loadChannelRecord(s.slotPath(name))
			if err != nil || ok && !s.retire(r, retiredEnded, now) {
				continue
			}
			_ = dialog.DropSession(name, rec.Holder)
		case state != launch.LeaseValid || rec.Binding != "":
		case listed:
			if b, ok := heldBinding(all, rec.Holder); ok {
				s.handover(name, rec, b, now)
				continue
			}
			fallthrough
		default:
			s.provisionSession(name, rec, now)
		}
	}
}

// heldBinding is the named binding, not proven ended, whose session runs in
// holder's process (dialog.HeldBy).
func heldBinding(all []binding.Binding, holder launch.HolderIdentity) (binding.Binding, bool) {
	for _, b := range all {
		if binding.ValidName(b.Name) != nil || !dialog.HeldBy(b, []launch.HolderIdentity{holder}) {
			continue
		}
		if state, _ := b.Liveness(); state != binding.Ended {
			return b, true
		}
	}
	return binding.Binding{}, false
}

// handover moves the active channel of session record name to binding b,
// so the session keeps its channel, and marks the record handed. A channel
// that is not active yet, or one that b cannot take (b has a record or an
// agent of its own), is retired instead, and b gets its own as usual.
func (s *Server) handover(name string, rec dialog.SessionRecord, b binding.Binding, now time.Time) {
	r, ok, err := loadChannelRecord(s.slotPath(name))
	if err != nil {
		return
	}
	if ok {
		cur, held, err := loadChannelRecord(s.slotPath(b.Name))
		switch {
		case err != nil:
			return
		case held && cur.Reg == r.Reg:
			// The move is written; only the old slot is left to remove.
			if s.removeDurably(s.slotPath(name)) != nil {
				return
			}
		case r.State == channelRetired:
			if _, moved := s.finishRetire(r, now); !moved {
				return
			}
		case held || r.State != channelActive || r.ChannelID == "" || !s.provisionable(b):
			if !s.retire(r, retiredMoved, now) {
				return
			}
		default:
			moved := r
			moved.Name, moved.Identity, moved.Label, moved.Handed = b.Name, b.SessionIdentity(), sessionAgentName(b), true
			if s.save(&moved, now) != nil || s.removeDurably(s.slotPath(name)) != nil {
				return
			}
			s.announceHandover(&moved, now)
		}
	}
	_ = dialog.HandOver(name, rec.Holder, b.Name)
}

// announceHandover gives a channel that moved to a binding the binding's
// label and says once that it now reaches the session. A failed call is
// tried again after channelStepGap.
func (s *Server) announceHandover(r *channelRecord, now time.Time) {
	if !r.Handed || r.ChannelID == "" || s.stopped() || r.LastError != "" && !due(r.LastTry, channelStepGap, now) {
		return
	}
	r.LastTry = now.UTC().Format(time.RFC3339)
	err := accepted(s.call("channels", "update", "--channel", r.ChannelID, "--name", r.Label))
	if err == nil {
		err = accepted(s.call("messages", "send", "--channel", r.ChannelID, "--content", "This channel now reaches "+r.Label+"; chat from Buzz works."))
	}
	if err != nil {
		r.LastError = err.Error()
	} else {
		r.Handed, r.LastError = false, ""
	}
	_ = s.save(r, now)
}

// provisionSession moves the channel record of session record name one or
// more steps toward active, as provision does for a binding.
func (s *Server) provisionSession(name string, rec dialog.SessionRecord, now time.Time) {
	r, ok, err := loadChannelRecord(s.slotPath(name))
	if err != nil {
		return
	}
	if ok && r.State == channelRetired {
		if _, moved := s.finishRetire(r, now); !moved {
			return
		}
		ok = false
	}
	id := sessionIdentity(rec)
	if ok && r.Identity != id {
		if !s.retire(r, retiredReplaced, now) {
			return
		}
		ok = false
	}
	if !ok {
		if s.stopped() {
			return
		}
		reg, err := newReg()
		if err != nil {
			return
		}
		r = channelRecord{Relay: normalizeRelay(buzzRelayURL), Machine: buzzSelf, Name: name, Identity: id,
			Label: rec.Label, Reg: reg, State: channelCreating}
		if s.save(&r, now) != nil {
			return
		}
	}
	s.advance(&r, now)
}

// sessionServes is stillServes for the record of a session record: the
// session record still names r's process, has its own channel, and that
// process is not proven gone.
func (s *Server) sessionServes(r channelRecord, now time.Time) bool {
	rec, ok, err := dialog.ReadSession(r.Name)
	switch {
	case err != nil:
		return false
	case !ok || rec.Binding != "":
		s.retire(r, retiredEnded, now)
		return false
	case sessionIdentity(rec) != r.Identity:
		s.retire(r, retiredReplaced, now)
		return false
	}
	if state, _ := launch.InspectHolder(rec.Holder); state == launch.LeaseStale {
		s.retire(r, retiredEnded, now)
		return false
	}
	return true
}

// handedDialogs are the session records handed to binding name: their open
// dialogs post in that binding's channel.
func handedDialogs(name string) []string {
	recs, err := dialog.Sessions()
	if err != nil {
		return nil
	}
	var out []string
	for n, rec := range recs {
		if rec.Binding == name {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
