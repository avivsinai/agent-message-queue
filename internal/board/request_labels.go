package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Wire schemas of on-demand classification. The caller runs the model and
// chooses when; the board only exports evidence and imports values.
const (
	ClassifySchema = "amq/board-requests-classify/v1"
	LabelsSchema   = "amq/board-request-labels/v1"
)

// ClassifyMessage is one whole message supplied to a classifier.
type ClassifyMessage struct {
	Key     string       `json:"key"`
	Role    string       `json:"role"`
	ID      string       `json:"id"`
	Thread  string       `json:"thread"`
	Kind    string       `json:"kind"`
	Created string       `json:"created"`
	From    string       `json:"from"`
	To      []string     `json:"to"`
	Sender  *Participant `json:"sender,omitempty"`
	Subject string       `json:"subject"`
	Body    string       `json:"body"`
	Refs    []string     `json:"refs,omitempty"`
	Labels  []string     `json:"labels,omitempty"`
}

// ClassifyCandidate is one request with a bounded, unclipped evidence
// slice. Omitted counts evidence messages left out of the slice; they
// still belong to the digest. An unclassifiable candidate takes unclear.
type ClassifyCandidate struct {
	MessageKey     string            `json:"message_key"`
	EvidenceDigest string            `json:"evidence_digest"`
	Messages       []ClassifyMessage `json:"messages"`
	Omitted        int               `json:"omitted"`
	Coverage       RequestCoverage   `json:"coverage"`
	Unclassifiable string            `json:"unclassifiable,omitempty"`
}

// ClassifyBatch is the export: Recent requests without an opinion from
// this classifier at their current digest.
type ClassifyBatch struct {
	Schema     string              `json:"schema"`
	ProjectID  string              `json:"project_id"`
	Classifier string              `json:"classifier"`
	Candidates []ClassifyCandidate `json:"candidates"`
	Remaining  int                 `json:"remaining"`
}

// LabelBatch is the import file.
type LabelBatch struct {
	Schema     string       `json:"schema"`
	ProjectID  string       `json:"project_id"`
	Classifier string       `json:"classifier"`
	Labels     []LabelEntry `json:"labels"`
}

// LabelEntry is one opinion in a LabelBatch.
type LabelEntry struct {
	MessageKey     string        `json:"message_key"`
	EvidenceDigest string        `json:"evidence_digest"`
	Value          string        `json:"value"`
	Quote          *RequestQuote `json:"quote,omitempty"`
}

// validClassifier keeps a classifier name printable and short enough to
// show as an attribution.
func validClassifier(name string) error {
	if name == "" || len(name) > 128 || strings.TrimSpace(name) != name || hasControl(name) {
		return errors.New("classifier needs a printable name of at most 128 bytes")
	}
	return nil
}

// classifyContext selects whole messages in evidence order (request, direct
// parent, direct replies, later thread messages, other referrers) while the
// slice stays within eight messages and 16 KiB. A message that does not fit
// is omitted, never clipped. A request that does not fit by itself is
// unclassifiable.
func (x requestIndex) classifyContext(key string) ([]ClassifyMessage, int, string) {
	evidence := x.evidence(key)
	var out []ClassifyMessage
	size := 2
	for _, e := range evidence {
		m := x.s.Messages[e[0]]
		cm := ClassifyMessage{Key: m.Key, Role: e[1], ID: m.ID, Thread: m.Thread, Kind: m.Kind, Created: m.Created, From: m.From, To: m.To, Subject: m.Subject, Body: m.Body, Refs: m.Refs, Labels: m.Labels}
		if p, ok := x.senderOf(m); ok {
			cm.Sender = &p
		}
		b, _ := json.Marshal(cm)
		if len(out) == requestContextMessages || size+len(b)+1 > requestContextBytes {
			if e[1] == "request" {
				return []ClassifyMessage{}, len(evidence), "the request message alone exceeds 16 KiB"
			}
			continue
		}
		size += len(b) + 1
		out = append(out, cm)
	}
	return out, len(evidence) - len(out), ""
}

// Classify exports up to next Recent requests that have no opinion from
// classifier at their current evidence digest, newest first. It is
// read-only.
func (s Snapshot) Classify(classifier string, next int, now time.Time) (ClassifyBatch, error) {
	if err := validClassifier(classifier); err != nil {
		return ClassifyBatch{}, err
	}
	if next < 1 {
		return ClassifyBatch{}, errors.New("--next must be at least 1")
	}
	x := newRequestIndex(s)
	batch := ClassifyBatch{Schema: ClassifySchema, ProjectID: s.ProjectID, Classifier: classifier, Candidates: []ClassifyCandidate{}}
	var pending []Request
	for _, key := range x.candidates() {
		r := x.request(key, now)
		if r.Age != RequestRecent {
			continue
		}
		if _, ok := s.RequestLabels[RequestLabelKey(key, r.EvidenceDigest, classifier)]; !ok {
			pending = append(pending, r)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool { return askNewer(pending[i].Message, pending[j].Message) })
	for i, r := range pending {
		if i == next {
			batch.Remaining = len(pending) - next
			break
		}
		messages, omitted, unclassifiable := x.classifyContext(r.Message.Key)
		batch.Candidates = append(batch.Candidates, ClassifyCandidate{MessageKey: r.Message.Key, EvidenceDigest: r.EvidenceDigest, Messages: messages, Omitted: omitted, Coverage: x.coverage, Unclassifiable: unclassifiable})
	}
	return batch, nil
}

// putLabels validates the whole batch against state, stores it, and drops
// stored labels whose message is gone, and labels of this batch's messages
// at another digest. Other labels stay: a digest that differs during one
// degraded sync can become current again. A label
// must name a request at its current digest. Every value except unclear
// needs an exact, nonempty quote from the body or subject of a message in
// the candidate's supplied slice; an unclassifiable candidate takes unclear
// without a quote. An identical label is accepted again; a different value
// for the same key is refused. It returns how many labels were new, or
// errUnchanged when nothing was added or dropped.
func putLabels(state *Snapshot, batch LabelBatch) (int, error) {
	if batch.Schema != LabelsSchema {
		return 0, fmt.Errorf("label file schema must be %s", LabelsSchema)
	}
	if batch.ProjectID != state.ProjectID {
		return 0, errors.New("label file names a different board project")
	}
	if err := validClassifier(batch.Classifier); err != nil {
		return 0, err
	}
	x := newRequestIndex(*state)
	now := time.Now()
	staged := map[string]RequestLabel{}
	for _, entry := range batch.Labels {
		m, ok := state.Messages[entry.MessageKey]
		if !ok || !isRequestKind(m.Kind) {
			return 0, fmt.Errorf("%s: not a request message in the published history", entry.MessageKey)
		}
		if _, ok := requestLabelValues[entry.Value]; !ok {
			return 0, fmt.Errorf("%s: value must be done, open, superseded, not_an_ask, or unclear", entry.MessageKey)
		}
		if digest := x.request(entry.MessageKey, now).EvidenceDigest; entry.EvidenceDigest != digest {
			return 0, fmt.Errorf("%s: evidence changed since export; export a fresh batch", entry.MessageKey)
		}
		messages, _, unclassifiable := x.classifyContext(entry.MessageKey)
		switch {
		case unclassifiable != "" && (entry.Value != "unclear" || entry.Quote != nil):
			return 0, fmt.Errorf("%s: unclassifiable; only unclear without a quote is accepted", entry.MessageKey)
		case entry.Quote == nil && entry.Value != "unclear":
			return 0, fmt.Errorf("%s: %s needs an exact quote", entry.MessageKey, entry.Value)
		case entry.Quote != nil:
			if err := checkQuote(*entry.Quote, messages); err != nil {
				return 0, fmt.Errorf("%s: %w", entry.MessageKey, err)
			}
		}
		label := RequestLabel{MessageKey: entry.MessageKey, EvidenceDigest: entry.EvidenceDigest, Classifier: batch.Classifier, Value: entry.Value, Quote: entry.Quote}
		key := RequestLabelKey(label.MessageKey, label.EvidenceDigest, label.Classifier)
		for _, prior := range []map[string]RequestLabel{staged, state.RequestLabels} {
			if existing, ok := prior[key]; ok && !sameLabel(existing, label) {
				return 0, fmt.Errorf("%s: a different opinion from %s is already recorded for this evidence; use a revised classifier name", entry.MessageKey, batch.Classifier)
			}
		}
		staged[key] = label
	}
	added, removed := 0, 0
	for key, label := range staged {
		if _, ok := state.RequestLabels[key]; ok {
			continue
		}
		if state.RequestLabels == nil {
			state.RequestLabels = map[string]RequestLabel{}
		}
		state.RequestLabels[key] = label
		added++
	}
	current := map[string]string{}
	for _, entry := range batch.Labels {
		current[entry.MessageKey] = entry.EvidenceDigest
	}
	for key, label := range state.RequestLabels {
		_, exists := state.Messages[label.MessageKey]
		digest, inBatch := current[label.MessageKey]
		if !exists || inBatch && label.EvidenceDigest != digest {
			delete(state.RequestLabels, key)
			removed++
		}
	}
	if added == 0 && removed == 0 {
		return 0, errUnchanged
	}
	return added, nil
}

func checkQuote(q RequestQuote, messages []ClassifyMessage) error {
	if strings.TrimSpace(q.Text) == "" {
		return errors.New("quote text is empty")
	}
	for _, m := range messages {
		if m.Key != q.MessageKey {
			continue
		}
		source := ""
		switch q.Field {
		case "body":
			source = m.Body
		case "subject":
			source = m.Subject
		default:
			return errors.New("quote field must be body or subject")
		}
		if !strings.Contains(source, q.Text) {
			return errors.New("quote is not exact text of the named message")
		}
		return nil
	}
	return errors.New("quote names a message that was not supplied")
}

func sameLabel(a, b RequestLabel) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// PutRequestLabels imports one label batch through the serialized snapshot
// publication. The whole batch commits or none of it does.
func (s Store) PutRequestLabels(ctx context.Context, batch LabelBatch) (int, error) {
	added := 0
	err := s.update(ctx, func(state *Snapshot) error {
		var err error
		added, err = putLabels(state, batch)
		return err
	})
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	return added, err
}
