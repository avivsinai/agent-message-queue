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
// and the record dropped. When a binding appears whose holder is that
// process (the session ran /amq-remote), the channel record moves to the
// binding's name, so the channel stays the session's, and the session
// record is dropped.

// sessionIdentity is the channel record identity of a session record.
func sessionIdentity(rec dialog.SessionRecord) string {
	h := rec.Holder
	return fmt.Sprintf("session:%s:%d:%s:%s", rec.Harness, h.PID, h.ProcessStart, h.BootID)
}

// provisionSessions works through every session record: retire and drop
// one whose process is proven gone, hand one over to the binding its
// process now holds, and provision the rest. A record whose process cannot
// be checked is left as it is.
func (s *Server) provisionSessions(all []binding.Binding, now time.Time) {
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
		switch state, _ := launch.InspectHolder(rec.Holder); state {
		case launch.LeaseStale:
			r, ok, err := loadChannelRecord(s.slotPath(name))
			if err != nil || ok && !s.retire(r, retiredEnded, now) {
				continue
			}
			_ = dialog.DropSession(name)
		case launch.LeaseValid:
			if b, ok := heldBinding(all, rec.Holder.PID); ok {
				s.handover(name, b, now)
			} else {
				s.provisionSession(name, rec, now)
			}
		}
	}
}

// heldBinding is the live named binding whose holder is pid.
func heldBinding(all []binding.Binding, pid int) (binding.Binding, bool) {
	for _, b := range all {
		if b.Holder.PID != pid || binding.ValidName(b.Name) != nil {
			continue
		}
		if state, _ := b.Liveness(); state == binding.Live {
			return b, true
		}
	}
	return binding.Binding{}, false
}

// handover moves the channel record of session record name to binding b,
// so the session keeps its channel, and drops the session record. When b
// already has a record, or has an agent of its own, the session's channel
// is retired instead.
func (s *Server) handover(name string, b binding.Binding, now time.Time) {
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
			// A handover that stopped after the move: only the old slot
			// is left to remove.
			if s.removeDurably(s.slotPath(name)) != nil {
				return
			}
		case r.State == channelRetired:
			if _, moved := s.finishRetire(r, now); !moved {
				return
			}
		case held || !s.provisionable(b):
			if !s.retire(r, retiredMoved, now) {
				return
			}
		default:
			moved := r
			moved.Name, moved.Identity = b.Name, b.SessionIdentity()
			if s.save(&moved, now) != nil || s.removeDurably(s.slotPath(name)) != nil {
				return
			}
		}
	}
	_ = dialog.DropSession(name)
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
// session record still names r's process and that process is not proven
// gone.
func (s *Server) sessionServes(r channelRecord, now time.Time) bool {
	rec, ok, err := dialog.ReadSession(r.Name)
	switch {
	case err != nil:
		return false
	case !ok:
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
