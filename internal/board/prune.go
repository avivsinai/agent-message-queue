package board

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// prunedIDHorizon bounds how long a pruned id is remembered. ReadHistory has
// no read window of its own, so a late delivery is assumed to arrive within
// 30 days of its creation; a message older than that which appears for the
// first time after a prune is not taken.
const prunedIDHorizon = 30 * 24 * time.Hour

// Pruned reports whether acquisition must not take the message with this id
// and creation time again. An unparsable time is pruned only by its id.
func (s *Snapshot) Pruned(id, created string) bool {
	if _, ok := s.PrunedIDs[id]; ok {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, created)
	return err == nil && at.Before(s.PrunedBefore)
}

// PrunedStamp reports whether a message id is pruned, judged by its id and
// the creation stamp that starts it, before its body is read. The stamp has
// millisecond precision, so only a stamp a whole millisecond before
// PrunedBefore counts; the commit decides the rest by creation time.
func (s *Snapshot) PrunedStamp(id string) bool {
	if _, ok := s.PrunedIDs[id]; ok {
		return true
	}
	stamp, _, ok := strings.Cut(id, "_")
	if !ok || s.PrunedBefore.IsZero() {
		return false
	}
	at, err := time.Parse("2006-01-02T15-04-05.000Z", stamp)
	return err == nil && at.Add(time.Millisecond).Before(s.PrunedBefore)
}

// pruneHistory removes the oldest messages that no task, suggestion or
// withdrawal holds until about excess encoded bytes are gone, with their
// observations, dismissals, cancellations and request labels. It records
// their ids in PrunedIDs, so acquisition does not bring them back, and
// retires their evidence under the current generation. It reports whether it
// removed anything.
func (s *Snapshot) pruneHistory(excess int) bool {
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
		created, err := time.Parse(time.RFC3339Nano, m.Created)
		if err != nil || held[key] {
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
		m.Body = ""
		b, _ := json.Marshal(m)
		o, _ := json.Marshal(s.Observations[c.key])
		freed += len(b) + len(o) + 2*len(c.key) - len(m.ID)
		for _, label := range labels[c.key] {
			l, _ := json.Marshal(s.RequestLabels[label])
			freed += len(l) + len(label)
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
		if created.Before(s.PrunedBefore) {
			delete(s.PrunedIDs, id)
		}
	}
	kept := map[string]bool{}
	for _, m := range s.Messages {
		kept[m.EvidenceHash] = true
	}
	if s.RetiredEvidence == nil {
		s.RetiredEvidence = map[string]uint64{}
	}
	for _, hash := range retired {
		if !kept[hash] {
			s.RetiredEvidence[hash] = s.Generation
		}
	}
	return true
}

// validatePruned repeats the read-time reference checks that removing
// messages can break.
func (s *Snapshot) validatePruned() error {
	for _, task := range s.Tasks {
		for _, key := range task.Attachments {
			if _, ok := s.Messages[key]; !ok {
				return errors.New("pruned an attached message")
			}
		}
	}
	if err := s.validateIdentityRecords(); err != nil {
		return err
	}
	return s.validateModelBindings()
}

// heldMessages are the message keys a task, suggestion or withdrawal names.
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
	return held
}
