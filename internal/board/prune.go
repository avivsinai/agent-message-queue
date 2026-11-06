package board

import (
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// pruneHistory removes the oldest messages that no task, suggestion or
// withdrawal holds until about excess encoded bytes are gone, with their
// observations, dismissals, cancellations and request labels. It raises
// HistoryFloor to the newest removed creation time, so acquisition does not
// bring them back, and retires their evidence under the current generation.
// It reports whether it removed anything.
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
	var retired []string
	freed := 0
	for i, c := range candidates {
		// Every message created at the floor goes, so the floor names exactly
		// what was pruned.
		if freed >= excess && !c.created.Equal(candidates[i-1].created) {
			break
		}
		m := s.Messages[c.key]
		m.Body = ""
		b, _ := json.Marshal(m)
		o, _ := json.Marshal(s.Observations[c.key])
		freed += len(b) + len(o) + 2*len(c.key) + 8
		for _, label := range labels[c.key] {
			l, _ := json.Marshal(s.RequestLabels[label])
			freed += len(l) + len(label)
			delete(s.RequestLabels, label)
		}
		delete(s.Messages, c.key)
		delete(s.Observations, c.key)
		delete(s.Dismissed, c.key)
		delete(s.ActionCancelled, c.key)
		retired = append(retired, m.EvidenceHash)
		if c.created.After(s.HistoryFloor) {
			s.HistoryFloor = c.created
		}
	}
	if len(retired) == 0 {
		return false
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

// belowFloor reports whether m is pruned history that must not return.
func (s *Snapshot) belowFloor(m Message) bool {
	if s.HistoryFloor.IsZero() {
		return false
	}
	created, err := time.Parse(time.RFC3339Nano, m.Created)
	return err == nil && !created.After(s.HistoryFloor)
}
