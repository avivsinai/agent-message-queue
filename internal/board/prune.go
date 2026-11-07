package board

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// prunedIDHorizon bounds how long a pruned id is remembered. ReadHistory has
// no read window of its own, so a late delivery is assumed to arrive within
// 30 days of its creation; a message older than that which appears for the
// first time after a prune is not taken.
const prunedIDHorizon = 30 * 24 * time.Hour

// Pruned reports whether acquisition must not take the message with this id
// again: the id was pruned, or the creation stamp that starts it is below
// the pruned floor.
func (s *Snapshot) Pruned(id string) bool {
	if _, ok := s.PrunedIDs[id]; ok {
		return true
	}
	at, ok := format.IDTime(id)
	return ok && s.belowPrunedFloor(at)
}

// belowPrunedFloor reports whether an id stamp lies a whole millisecond
// before PrunedBefore. Stamps have millisecond precision, so a stamp within
// the floor's millisecond is not below it.
func (s *Snapshot) belowPrunedFloor(at time.Time) bool {
	return !s.PrunedBefore.IsZero() && at.Add(time.Millisecond).Before(s.PrunedBefore)
}

// pruneHistory removes the oldest messages that heldMessages does not hold,
// by the stamp that starts their ids, until about excess bytes are gone, with their
// observations, dismissals, cancellations and request labels. It records
// their ids in PrunedIDs, so acquisition does not bring them back, and
// retires their evidence under the current generation. It reports whether it
// removed anything. cost is the bytes a message frees; nil counts its
// encoding in the snapshot.
func (s *Snapshot) pruneHistory(excess int, cost func(Message) int) bool {
	if excess <= 0 {
		return false
	}
	held := s.heldMessages()
	type candidate struct {
		key     string
		created time.Time
	}
	var candidates []candidate
	for key, m := range s.Messages {
		created, ok := format.IDTime(m.ID)
		if !ok || held[key] {
			continue
		}
		candidates = append(candidates, candidate{key, created})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].created.Equal(candidates[j].created) {
			return candidates[i].created.Before(candidates[j].created)
		}
		return candidates[i].key < candidates[j].key
	})
	labels := map[string][]string{}
	for key, label := range s.RequestLabels {
		labels[label.MessageKey] = append(labels[label.MessageKey], key)
	}
	if s.PrunedIDs == nil {
		s.PrunedIDs = map[string]time.Time{}
	}
	var retired []string
	freed := 0
	for _, c := range candidates {
		if freed >= excess {
			break
		}
		m := s.Messages[c.key]
		if cost != nil {
			freed += cost(m)
		} else {
			m.Body = ""
			b, _ := json.Marshal(m)
			o, _ := json.Marshal(s.Observations[c.key])
			freed += len(b) + len(o) + 2*len(c.key) - len(m.ID)
		}
		for _, label := range labels[c.key] {
			if cost == nil {
				l, _ := json.Marshal(s.RequestLabels[label])
				freed += len(l) + len(label)
			}
			delete(s.RequestLabels, label)
		}
		delete(s.Messages, c.key)
		delete(s.Observations, c.key)
		delete(s.Dismissed, c.key)
		delete(s.ActionCancelled, c.key)
		s.PrunedIDs[m.ID] = c.created
		retired = append(retired, m.EvidenceHash)
	}
	if len(retired) == 0 {
		return false
	}
	// Pruned ids older than the horizon give way to one time bound, so the
	// set stays as small as the horizon's history.
	if horizon := s.PublishedAt.Add(-prunedIDHorizon); horizon.After(s.PrunedBefore) {
		s.PrunedBefore = horizon
	}
	for id, created := range s.PrunedIDs {
		if s.belowPrunedFloor(created) {
			delete(s.PrunedIDs, id)
		}
	}
	s.retireEvidence(retired)
	return true
}

// retireEvidence marks each hash that no kept message names for removal.
// The publication that carries the mark stamps it with its generation.
func (s *Snapshot) retireEvidence(hashes []string) {
	if len(hashes) == 0 {
		return
	}
	kept := map[string]bool{}
	for _, m := range s.Messages {
		kept[m.EvidenceHash] = true
	}
	if s.RetiredEvidence == nil {
		s.RetiredEvidence = map[string]uint64{}
	}
	for _, hash := range hashes {
		if !kept[hash] && hash != "" {
			s.RetiredEvidence[hash] = 0
		}
	}
}

// heldMessages are the message keys pruning keeps: those a task,
// suggestion or withdrawal names, and the opener of every open obligation
// with every message whose refs name it. An opener whose id stamp lies
// before the pruned-id horizon is not held, nor are its referrers, so the
// held set does not only grow.
func (s *Snapshot) heldMessages() map[string]bool {
	held := map[string]bool{}
	for _, task := range s.Tasks {
		for _, key := range task.Attachments {
			held[key] = true
		}
		for _, b := range task.ModelAttachments {
			held[b.Unit.MessageKey] = true
		}
	}
	for _, u := range s.Suggestions.Units {
		held[u.MessageKey] = true
	}
	for _, p := range s.Suggestions.Proposals {
		held[p.Unit.MessageKey] = true
	}
	for key, withdrawal := range s.Withdrawals {
		held[key] = true
		held[withdrawal] = true
	}
	project := ""
	for _, nr := range s.Input.Results {
		if nr.Name == "amq" && nr.Result.AMQ != nil {
			project = nr.Result.AMQ.Project
		}
	}
	type namespace struct {
		entries []thread.Entry
		keys    []string
	}
	namespaces := map[string]*namespace{}
	for key, m := range s.Messages {
		ns := namespaces[m.Namespace]
		if ns == nil {
			ns = &namespace{}
			namespaces[m.Namespace] = ns
		}
		ns.entries = append(ns.entries, obligationEntry(m, project))
		ns.keys = append(ns.keys, key)
	}
	horizon := s.PublishedAt.Add(-prunedIDHorizon)
	for _, ns := range namespaces {
		open := map[string]bool{}
		// No roots: the reduction judges the snapshot and reads no mailbox.
		for _, ob := range thread.ReduceObligations(nil, ns.entries, time.Time{}, reservedHuman).Obligations {
			if at, ok := format.IDTime(ob.Opener.ID); ob.Open() && (!ok || !at.Before(horizon)) {
				open[ob.Opener.ID] = true
			}
		}
		for i, e := range ns.entries {
			if open[e.ID] || slices.ContainsFunc(e.Refs, func(ref string) bool { return open[ref] }) {
				held[ns.keys[i]] = true
			}
		}
	}
	return held
}

// obligationEntry is the obligation reduction's view of m on a board of
// project. A copy names the agents of its root that hold the thread, not the
// mailbox that holds m, so a sender in project reads as an outbox copy in its
// reply_to session or else the one session whose agents name it, and each
// recipient outside project's outbound routes reads as an inbox copy in the
// one session, outside the sender's routed session, whose agents name it.
// Anything else is left to the routing fields and the local fallback.
func obligationEntry(m Message, project string) thread.Entry {
	e := thread.Entry{ID: m.ID, From: m.From, To: m.To, Thread: m.Thread, Created: m.Created, Kind: m.Kind, Labels: m.Labels, Refs: m.Refs, ReplyTo: m.ReplyTo, ReplyProject: m.ReplyProject, FromProject: m.FromProject}
	if at, ok := messageTime(m); ok {
		e.RawTime = at
	}
	_, routedSession, routed := strings.Cut(m.ReplyTo, "@")
	if m.ReplyProject == "" || project != "" && m.ReplyProject == project {
		if routed {
			e.Occurrences = append(e.Occurrences, thread.Occurrence{Session: routedSession, Agent: m.From, Dir: fsq.MailboxOutboxSent})
		} else if session, ok := rootOf(m, m.From, nil); ok {
			e.Occurrences = append(e.Occurrences, thread.Occurrence{Session: session, Agent: m.From, Dir: fsq.MailboxOutboxSent})
		}
	}
	if m.ReplyProject == "" || project != "" && m.ReplyProject != project {
		var skip *string
		if routed && m.ReplyProject == "" {
			skip = &routedSession
		}
		for _, handle := range m.To {
			if session, ok := rootOf(m, handle, skip); ok {
				e.Occurrences = append(e.Occurrences, thread.Occurrence{Session: session, Agent: handle, Dir: fsq.MailboxInboxCur})
			}
		}
	}
	if len(e.Occurrences) == 0 {
		e.Occurrences = []thread.Occurrence{{Session: m.Session}}
	}
	return e
}
