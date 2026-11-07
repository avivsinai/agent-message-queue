package board

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
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
// encoding in the snapshot. Evidence is retired under generation gen, the
// generation that publishes the prune.
func (s *Snapshot) pruneHistory(excess int, cost func(Message) int, gen uint64) bool {
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
	s.retireEvidence(retired, gen)
	return true
}

// retireEvidence marks each hash that no kept message names for removal
// once no kept generation from gen on can name it.
func (s *Snapshot) retireEvidence(hashes []string, gen uint64) {
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
			s.RetiredEvidence[hash] = gen
		}
	}
}

// validatePruned confirms that every message heldMessages names is still
// present, so a prune never publishes a store that needs recovery.
func (s *Snapshot) validatePruned() error {
	for key := range s.heldMessages() {
		if _, ok := s.Messages[key]; !ok {
			return errors.New("pruned a held message")
		}
	}
	return nil
}

// heldMessages are the message keys pruning keeps: those a task,
// suggestion or withdrawal names, every request with a recipient that has no
// observed direct reply, and every message that refers to such a request.
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
	x := newRequestIndex(*s)
	for key, m := range s.Messages {
		if !isRequestKind(m.Kind) {
			continue
		}
		for _, r := range x.request(key, s.PublishedAt).Recipients {
			if len(r.DirectReplies) == 0 {
				held[key] = true
				for _, reply := range x.referrer[m.Namespace+"#"+m.ID] {
					held[reply] = true
				}
				break
			}
		}
	}
	return held
}
