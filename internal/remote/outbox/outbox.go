// Package outbox keeps one durable operation record per intended relay
// write. A record holds the exact signed event for its key and the outcome
// of its last attempt, so a restart resends the same event id and never
// signs a second event for one intention.
//
// Invariants:
//   - A record never changes its event bytes or its event id.
//   - The outcome moves to unknown, durably, before the caller sends the
//     event.
//   - Accepted is terminal.
//   - An unreadable record is an error, never treated as absent.
//
// One process owns a directory at a time; the caller holds the lock that
// makes it the single writer. Within the process an Outbox serializes its
// own changes.
package outbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

//   - A record is removed only by Prune, and never while its outcome is
//     unknown: only its own event can be sent again without a second
//     delivery.
//
// OutcomeOf maps the result of relay.Conn.Publish to an outcome.
func OutcomeOf(err error) buzzop.Outcome {
	switch {
	case err == nil:
		return buzzop.Accepted
	case errors.Is(err, relay.ErrRejected):
		return buzzop.Rejected
	case errors.Is(err, relay.ErrUnknownDelivery):
		return buzzop.Unknown
	default:
		return buzzop.NotSent
	}
}

// Bounds of one record. A record holds one event of at most 64 KiB of
// content, which JSON escaping can grow several times.
const (
	maxRecordBytes = 1 << 20
	maxReasonBytes = 512
)

// Record is one operation: the signed event owed for Key and what is known
// about sending it. Times are Unix seconds.
type Record struct {
	Key     string          `json:"key"`
	Reg     string          `json:"reg"`
	Kind    nostr.Kind      `json:"kind"`
	EventID string          `json:"event_id"`
	Event   json.RawMessage `json:"event"`
	Outcome buzzop.Outcome  `json:"outcome"`
	// Attempts counts the sends begun.
	Attempts int `json:"attempts"`
	// NextAttempt is the time the caller gave for the next attempt; zero
	// once accepted. The outbox stores it and never acts on it.
	NextAttempt int64  `json:"next_attempt"`
	Reason      string `json:"reason,omitempty"`
	// Seq is the record's place in preparation order in its directory.
	Seq      int64 `json:"seq"`
	Prepared int64 `json:"prepared"`
	Settled  int64 `json:"settled,omitempty"`
}

// SignedEvent decodes the stored event.
func (r Record) SignedEvent() (nostr.Event, error) {
	var evt nostr.Event
	if err := json.Unmarshal(r.Event, &evt); err != nil {
		return nostr.Event{}, fmt.Errorf("outbox record %q event: %w", r.Key, err)
	}
	return evt, nil
}

// Outbox is the set of operation records in one directory.
type Outbox struct {
	dir string

	mu      sync.Mutex
	lastSeq int64 // highest sequence handed out; -1 until loaded
}

// Open creates or opens the outbox in dir.
func Open(dir string) (*Outbox, error) {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Outbox{dir: dir, lastSeq: -1}, nil
}

// Prepare records evt, signed, as the event owed for key, with outcome
// not_sent and the next sequence number. If key already has a record, that
// record is returned unchanged: the caller sends the stored event, never a
// new one.
func (o *Outbox) Prepare(key, reg string, evt nostr.Event, now time.Time) (Record, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if stored, ok, err := o.get(key); err != nil || ok {
		return stored, err
	}
	if !evt.CheckID() || !evt.VerifySignature() {
		return Record{}, fmt.Errorf("outbox record %q: event is not signed", key)
	}
	raw, err := evt.MarshalJSON()
	if err != nil {
		return Record{}, err
	}
	seq, err := o.nextSeq()
	if err != nil {
		return Record{}, err
	}
	r := Record{
		Key: key, Reg: reg, Kind: evt.Kind, EventID: evt.ID.Hex(), Event: raw,
		Outcome: buzzop.NotSent, NextAttempt: now.Unix(), Seq: seq, Prepared: now.Unix(),
	}
	data, err := encode(r)
	if err != nil {
		return Record{}, err
	}
	path := filepath.Join(o.dir, keyFile(key))
	if _, err := fsq.CreateNewFile(path, data, 0o600); err != nil {
		return Record{}, err
	}
	stored, _, err := o.get(key)
	return stored, err
}

// BeginAttempt durably records that a send of key's event starts: attempts
// grows by one and the outcome is unknown. The caller sends only after it
// returns. An accepted record is never sent again.
func (o *Outbox) BeginAttempt(key string, now time.Time) (Record, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.mustGet(key)
	if err != nil {
		return Record{}, err
	}
	if r.Outcome == buzzop.Accepted {
		return Record{}, fmt.Errorf("outbox record %q is already accepted", key)
	}
	r.Attempts++
	r.Outcome = buzzop.Unknown
	return r, o.put(r)
}

// Settle records the outcome of key's last attempt at now, and next as the
// time of its next attempt; the caller owns the retry policy. An accepted
// record does not change and has no next attempt.
func (o *Outbox) Settle(key string, outcome buzzop.Outcome, reason string, now, next time.Time) (Record, error) {
	switch outcome {
	case buzzop.NotSent, buzzop.Accepted, buzzop.Rejected, buzzop.Unknown:
	default:
		return Record{}, fmt.Errorf("outbox outcome %q is not one of not_sent, accepted, rejected, unknown", outcome)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.mustGet(key)
	if err != nil || r.Outcome == buzzop.Accepted {
		return r, err
	}
	if len(reason) > maxReasonBytes {
		reason = strings.ToValidUTF8(reason[:maxReasonBytes], "") + "…"
	}
	r.Outcome, r.Reason, r.Settled, r.NextAttempt = outcome, reason, now.Unix(), next.Unix()
	if outcome == buzzop.Accepted {
		r.NextAttempt = 0
	}
	return r, o.put(r)
}

// Get returns key's record, if one exists.
func (o *Outbox) Get(key string) (Record, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.get(key)
}

// Unsettled returns every record that is not accepted, in sequence order.
// The caller reads NextAttempt to tell which are due, and sees the records
// that are not, so it can hold a later record of a registration behind an
// earlier one.
func (o *Outbox) Unsettled() ([]Record, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	all, err := o.scan()
	if err != nil {
		return nil, err
	}
	var open []Record
	for _, r := range all {
		if r.Outcome != buzzop.Accepted {
			open = append(open, r)
		}
	}
	return open, nil
}

// Prune removes every record whose outcome is not unknown and whose last
// change (its settlement, or its preparation when it has none) is older
// than keep, and returns how many it removed. The caller gives a keep
// longer than any time after which a writer asks again for an accepted key.
func (o *Outbox) Prune(now time.Time, keep time.Duration) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	all, err := o.scan()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, r := range all {
		if r.Outcome == buzzop.Unknown || now.Sub(time.Unix(max(r.Prepared, r.Settled), 0)) <= keep {
			continue
		}
		if err := os.Remove(filepath.Join(o.dir, keyFile(r.Key))); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// Stats is what a reader that does not own an outbox may know of it.
type Stats struct {
	Records int
	// OldestUnknown is the last change, in Unix seconds, of the record with
	// outcome unknown that changed longest ago; zero when there is none.
	OldestUnknown int64
}

// Stat reads the records in dir and changes nothing. A directory that does
// not exist has no record. A record that its owner removes during the read
// is not counted.
func Stat(dir string) (Stats, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return Stats{}, nil
	}
	if err != nil {
		return Stats{}, err
	}
	var stats Stats
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		r, err := read(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Stats{}, err
		}
		stats.Records++
		if r.Outcome == buzzop.Unknown {
			t := max(r.Prepared, r.Settled)
			if stats.OldestUnknown == 0 || t < stats.OldestUnknown {
				stats.OldestUnknown = t
			}
		}
	}
	return stats, nil
}

// nextSeq returns the next sequence number. The records are its durable
// state: the first call after Open continues after the highest one.
func (o *Outbox) nextSeq() (int64, error) {
	if o.lastSeq < 0 {
		all, err := o.scan()
		if err != nil {
			return 0, err
		}
		o.lastSeq = 0
		for _, r := range all {
			o.lastSeq = max(o.lastSeq, r.Seq)
		}
	}
	o.lastSeq++
	return o.lastSeq, nil
}

// scan returns every record in sequence order.
func (o *Outbox) scan() ([]Record, error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, err
	}
	var all []Record
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		r, err := read(filepath.Join(o.dir, name))
		if err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	return all, nil
}

func (o *Outbox) get(key string) (Record, bool, error) {
	r, err := read(filepath.Join(o.dir, keyFile(key)))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return r, true, nil
}

func (o *Outbox) mustGet(key string) (Record, error) {
	r, ok, err := o.get(key)
	if err == nil && !ok {
		err = fmt.Errorf("outbox record %q: %w", key, os.ErrNotExist)
	}
	return r, err
}

// put atomically replaces r's file. The directory above the outbox anchors
// the write, so no directory from the outbox down is followed as a link.
func (o *Outbox) put(r Record) error {
	data, err := encode(r)
	if err != nil {
		return err
	}
	_, err = fsq.WriteStateFile(filepath.Dir(o.dir), filepath.Join(filepath.Base(o.dir), keyFile(r.Key)), data, 0o600)
	return err
}

// encode writes r without HTML escaping, so the stored event bytes are the
// bytes the event marshals to.
func encode(r Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	data := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(data) > maxRecordBytes {
		return nil, fmt.Errorf("outbox record %q is %d bytes, over %d", r.Key, len(data), maxRecordBytes)
	}
	return data, nil
}

// read decodes the record at path and checks that its key names the file.
func read(path string) (Record, error) {
	raw, err := fsq.ReadRegularNoFollowBounded(path, maxRecordBytes)
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("outbox record %s is unreadable: %w", filepath.Base(path), err)
	}
	if keyFile(r.Key) != filepath.Base(path) {
		return Record{}, fmt.Errorf("outbox record %s does not hold its own key", filepath.Base(path))
	}
	return r, nil
}

// keyFile names a record by the SHA-256 of its key, so no key text chooses
// a file path.
func keyFile(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}
