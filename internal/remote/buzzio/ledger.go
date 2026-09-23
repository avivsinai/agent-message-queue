// Package buzzio is the Buzz DM edge of the remote endpoint (bead 611.16,
// relay design §4). This file is its durable ledger: ingress claims written
// before any command reaches the endpoint, prepared signed output written
// before any transmission, and the request-to-row receipt mapping. Every
// record is created once and then read back verbatim, so a restart,
// redelivery or duplicate subscription replays the same decision and the
// same signed bytes instead of making new ones.
package buzzio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// maxRecordBytes bounds one ledger file (a claim or a prepared event).
const maxRecordBytes = 128 << 10

// Ledger is the edge's on-disk state under <stateDir>/buzz. The endpoint's
// single-writer lock owns the directory; the ledger adds no lock of its own.
type Ledger struct {
	dir string
}

// OpenLedger creates or opens <stateDir>/buzz/{ingress,outbox,receipts}.
func OpenLedger(stateDir string) (*Ledger, error) {
	l := &Ledger{dir: filepath.Join(stateDir, "buzz")}
	for _, sub := range []string{"ingress", "outbox", "receipts"} {
		if err := os.MkdirAll(filepath.Join(l.dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// Claim is one owner command, fixed when first seen. RequestID is derived
// from the signed event, so a retried import can only ever address the same
// request.
type Claim struct {
	EventID string `json:"event_id"`
	Owner   string `json:"owner"`
	Body    string `json:"body"`
	Relay   string `json:"relay"`
	Channel string `json:"channel"`
	// DMChannel and NativeSession complete the share binding the claim was
	// made under: a mention's source channel is not its destination, and a
	// replacement native session never inherits old work (codex #866 r3 #2).
	DMChannel     string          `json:"dm_channel"`
	NativeSession string          `json:"native_session"`
	Op            string          `json:"op"`
	RequestID     string          `json:"request_id,omitempty"`
	Target        string          `json:"target"`
	Epoch         string          `json:"epoch,omitempty"`
	NotAfter      string          `json:"not_after,omitempty"`
	Command       json.RawMessage `json:"command"`
	CreatedAt     int64           `json:"created_at"`
}

// Claim records c if its event id is new, and returns the stored claim and
// whether this call created it. An existing claim is returned verbatim: a
// changed manifest, epoch or clock never retargets it.
func (l *Ledger) Claim(c Claim) (Claim, bool, error) {
	if !validHexID(c.EventID) {
		return Claim{}, false, fmt.Errorf("claim: event id %q is not 64 lowercase hex", c.EventID)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return Claim{}, false, err
	}
	stored, created, err := createOnce(filepath.Join(l.dir, "ingress"), c.EventID+".json", raw)
	if err != nil {
		return Claim{}, false, err
	}
	var out Claim
	if err := json.Unmarshal(stored, &out); err != nil {
		return Claim{}, false, fmt.Errorf("claim %s is unreadable: %w", c.EventID, err)
	}
	return out, created, nil
}

// Settlement is the decision an owner command reached, recorded once after
// the endpoint answered it. A redelivered event with a settlement is never
// sent to the endpoint again, so a busy rejection the owner already saw
// cannot later turn into execution (codex #866 r1 #4). A claim without a
// settlement is an import interrupted before its outcome was known; it is
// recovered by re-sending the same command.
type Settlement struct {
	Op         string `json:"op"`
	RequestRef string `json:"request_ref,omitempty"`
	State      string `json:"state,omitempty"`
}

// Settle records s for eventID once and returns the stored settlement.
func (l *Ledger) Settle(eventID string, s Settlement) (Settlement, error) {
	if !validHexID(eventID) {
		return Settlement{}, fmt.Errorf("settle: event id %q is not 64 lowercase hex", eventID)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return Settlement{}, err
	}
	stored, _, err := createOnce(filepath.Join(l.dir, "ingress"), eventID+".settled.json", raw)
	if err != nil {
		return Settlement{}, err
	}
	var out Settlement
	if err := json.Unmarshal(stored, &out); err != nil {
		return Settlement{}, fmt.Errorf("settlement %s is unreadable: %w", eventID, err)
	}
	return out, nil
}

// Settled returns eventID's settlement, if one was recorded.
func (l *Ledger) Settled(eventID string) (Settlement, bool, error) {
	if !validHexID(eventID) {
		return Settlement{}, false, nil
	}
	raw, err := readBounded(filepath.Join(l.dir, "ingress", eventID+".settled.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Settlement{}, false, nil
	}
	if err != nil {
		return Settlement{}, false, err
	}
	var out Settlement
	if err := json.Unmarshal(raw, &out); err != nil {
		return Settlement{}, false, fmt.Errorf("settlement %s is unreadable: %w", eventID, err)
	}
	return out, true, nil
}

// Outbound is one prepared output: exact signed event bytes owed to the
// relay, keyed by the obligation it satisfies (a direct answer to an
// ingress event, or a request's result row). Revision is the snapshot
// revision a row event shows.
type Outbound struct {
	Key      string          `json:"key"`
	Event    json.RawMessage `json:"event"`
	Revision int             `json:"revision,omitempty"`
	// Binding is the share the output was prepared under; Flush sends it
	// only while the current share is the same (codex #866 r2 #4).
	Binding  ShareBinding `json:"binding"`
	Accepted bool         `json:"accepted"`
}

// ShareBinding is the full identity of a share: an output, claim or
// receipt from any other binding is never sent, replayed or redirected.
type ShareBinding struct {
	Relay         string `json:"relay"`
	Owner         string `json:"owner"`
	Body          string `json:"body"`
	Channel       string `json:"channel"`
	Target        string `json:"target"`
	NativeSession string `json:"native_session"`
}

// Prepare records the signed event bytes for key before transmission and
// returns what is stored. If key was prepared before, the stored bytes win
// and are returned unchanged: a retry resends the same event id, never a
// re-signed one.
func (l *Ledger) Prepare(key string, event json.RawMessage, b ShareBinding) (Outbound, error) {
	return l.PrepareRevision(key, event, 0, b)
}

// PrepareRevision is Prepare for a row event that shows revision.
func (l *Ledger) PrepareRevision(key string, event json.RawMessage, revision int, b ShareBinding) (Outbound, error) {
	o := Outbound{Key: key, Event: event, Revision: revision, Binding: b}
	raw, err := json.Marshal(o)
	if err != nil {
		return Outbound{}, err
	}
	stored, _, err := createOnce(filepath.Join(l.dir, "outbox"), keyFile(key), raw)
	if err != nil {
		return Outbound{}, err
	}
	return l.readOutbound(stored)
}

// MarkAccepted records the relay's matching positive OK for key.
func (l *Ledger) MarkAccepted(key string) error {
	dir := filepath.Join(l.dir, "outbox")
	stored, err := readBounded(filepath.Join(dir, keyFile(key)))
	if err != nil {
		return err
	}
	o, err := l.readOutbound(stored)
	if err != nil {
		return err
	}
	if o.Accepted {
		return nil
	}
	o.Accepted = true
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(dir, keyFile(key), raw, 0o600)
	return err
}

// Pending returns prepared outputs not yet accepted, in key order.
func (l *Ledger) Pending() ([]Outbound, error) {
	dir := filepath.Join(l.dir, "outbox")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Outbound
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		raw, err := readBounded(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		o, err := l.readOutbound(raw)
		if err != nil {
			return nil, err
		}
		if !o.Accepted {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Prepared returns the output prepared for key, if any.
func (l *Ledger) Prepared(key string) (Outbound, bool, error) {
	raw, err := readBounded(filepath.Join(l.dir, "outbox", keyFile(key)))
	if errors.Is(err, os.ErrNotExist) {
		return Outbound{}, false, nil
	}
	if err != nil {
		return Outbound{}, false, err
	}
	o, err := l.readOutbound(raw)
	return o, err == nil, err
}

func (l *Ledger) readOutbound(raw []byte) (Outbound, error) {
	var o Outbound
	if err := json.Unmarshal(raw, &o); err != nil {
		return Outbound{}, fmt.Errorf("outbox record is unreadable: %w", err)
	}
	return o, nil
}

// Receipt maps a request to its one result row in the owner DM: the row's
// original event id, and the newest second an edit of it was dated, so the
// next edit is always strictly newer (Buzz orders edits by seconds).
type Receipt struct {
	RequestRef  string `json:"request_ref"`
	RootEventID string `json:"root_event_id"`
	LastEditAt  int64  `json:"last_edit_at"`
	Revision    int    `json:"revision"`
	// The share binding and native address the request was submitted
	// under: cancel needs the target and epoch, and a changed share never
	// adopts or redirects another binding's request (codex #866 r1 #1, #6).
	Owner         string `json:"owner"`
	Body          string `json:"body"`
	Relay         string `json:"relay"`
	Channel       string `json:"channel"`
	Target        string `json:"target"`
	Epoch         string `json:"epoch"`
	NativeSession string `json:"native_session"`
}

// PutReceipt writes a request's receipt (created when the request is
// submitted, updated as its row is prepared). The row-to-request mapping
// is written first, so a crash between the two leaves the reaction lookup
// in place and the next PutReceipt completes the receipt (codex #866 r1 #5).
func (l *Ledger) PutReceipt(r Receipt) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := l.ensureRowMap(r); err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(filepath.Join(l.dir, "receipts"), keyFile("ref/"+r.RequestRef), raw, 0o600)
	return err
}

// ensureRowMap writes the row-to-request mapping of r's root row if it is
// missing.
func (l *Ledger) ensureRowMap(r Receipt) error {
	if r.RootEventID == "" {
		return nil
	}
	if ref, ok, err := l.RequestForRow(r.RootEventID); err != nil || (ok && ref == r.RequestRef) {
		return err
	}
	_, err := fsq.WriteFileAtomic(filepath.Join(l.dir, "receipts"), keyFile("row/"+r.RootEventID), []byte(r.RequestRef), 0o600)
	return err
}

// RequestForRow returns the request whose result row has this event id.
func (l *Ledger) RequestForRow(rowEventID string) (string, bool, error) {
	raw, err := readBounded(filepath.Join(l.dir, "receipts", keyFile("row/"+rowEventID)))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// ReceiptFor returns the receipt for a request, if one exists.
func (l *Ledger) ReceiptFor(requestRef string) (Receipt, bool, error) {
	raw, err := readBounded(filepath.Join(l.dir, "receipts", keyFile("ref/"+requestRef)))
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return Receipt{}, false, fmt.Errorf("receipt %s is unreadable: %w", requestRef, err)
	}
	return r, true, nil
}

// createOnce writes data as dir/name only if name does not exist, and
// returns the file's contents and whether this call created it. The data is
// written to a synced temp file and hard-linked into place, so the create is
// atomic and a concurrent or earlier writer always wins verbatim.
func createOnce(dir, name string, data []byte) ([]byte, bool, error) {
	if len(data) > maxRecordBytes {
		return nil, false, fmt.Errorf("ledger record %s is %d bytes, over %d", name, len(data), maxRecordBytes)
	}
	final := filepath.Join(dir, name)
	if existing, err := readBounded(final); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%d", name, time.Now().UnixNano()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return nil, false, werr
	}
	lerr := os.Link(tmp, final)
	_ = os.Remove(tmp)
	if lerr != nil && !errors.Is(lerr, os.ErrExist) {
		return nil, false, lerr
	}
	if err := fsq.SyncDir(dir); err != nil {
		return nil, false, err
	}
	stored, err := readBounded(final)
	if err != nil {
		return nil, false, err
	}
	return stored, lerr == nil, nil
}

func readBounded(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxRecordBytes {
		return nil, fmt.Errorf("%s is over %d bytes", path, maxRecordBytes)
	}
	return raw, nil
}

// keyFile names a record by the hash of its key, so no key text chooses a
// file path.
func keyFile(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}

func validHexID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
