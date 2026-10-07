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
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// maxRecordBytes bounds one ledger file (a claim or a prepared event).
const maxRecordBytes = 128 << 10

// Ledger is the edge's on-disk state under <stateDir>/buzz. The endpoint's
// single-writer lock owns the directory; the ledger adds no file lock of
// its own, only seqMu, which serializes outbox sequence numbers in process.
type Ledger struct {
	dir string

	seqMu   sync.Mutex
	lastSeq int64 // highest outbox sequence handed out; -1 until loaded
}

// OpenLedger creates or opens <stateDir>/buzz/{ingress,outbox,receipts}.
func OpenLedger(stateDir string) (*Ledger, error) {
	l := &Ledger{dir: filepath.Join(stateDir, "buzz"), lastSeq: -1}
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

// ClaimFor returns the stored claim for an event, if one exists.
func (l *Ledger) ClaimFor(eventID string) (Claim, bool, error) {
	if !validHexID(eventID) {
		return Claim{}, false, nil
	}
	raw, err := readBounded(filepath.Join(l.dir, "ingress", eventID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, err
	}
	var out Claim
	if err := json.Unmarshal(raw, &out); err != nil {
		return Claim{}, false, fmt.Errorf("claim %s is unreadable: %w", eventID, err)
	}
	return out, true, nil
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
	// Seq is the output's place in preparation order, fixed when it is
	// first prepared; zero is a record from a build before sequences.
	Seq int64 `json:"seq,omitempty"`
	// Binding is the share the output was prepared under; Flush sends it
	// only while the current share is the same (codex #866 r2 #4).
	Binding  ShareBinding `json:"binding"`
	Accepted bool         `json:"accepted"`
	// AcceptedAt is the second the relay's positive OK was seen: a typed
	// answer must be later to refer to this output (611.42.7).
	AcceptedAt int64 `json:"accepted_at,omitempty"`
	// Refused is the relay's last negative OK for an output still owed:
	// a refusal describes that attempt only, so the output is retried after
	// a backoff and is never given up (agent-message-queue-611.58).
	Refused *Refusal `json:"refused,omitempty"`
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
	if stored, ok, err := l.Prepared(key); err != nil || ok {
		return stored, err
	}
	seq, err := l.nextSeq()
	if err != nil {
		return Outbound{}, err
	}
	o := Outbound{Key: key, Event: event, Revision: revision, Binding: b, Seq: seq}
	raw, err := json.Marshal(o)
	if err != nil {
		return Outbound{}, err
	}
	stored, _, err := createOnce(filepath.Join(l.dir, "outbox"), keyFile(key), raw)
	if err != nil {
		return Outbound{}, err
	}
	return readOutbound(stored)
}

// nextSeq returns the next outbox sequence number. The outbox records are
// its durable state: the first call after open continues after the highest
// stored one.
func (l *Ledger) nextSeq() (int64, error) {
	l.seqMu.Lock()
	defer l.seqMu.Unlock()
	if l.lastSeq < 0 {
		all, err := scanOutbox(filepath.Join(l.dir, "outbox"), func(Outbound) bool { return true })
		if err != nil {
			return 0, err
		}
		l.lastSeq = 0
		for _, o := range all {
			l.lastSeq = max(l.lastSeq, o.Seq)
		}
	}
	l.lastSeq++
	return l.lastSeq, nil
}

// Refusal is an owed output's refusal history: how many attempts the relay
// refused, its last reason (relay words), and when the next attempt is due.
type Refusal struct {
	Attempts int    `json:"attempts"`
	Reason   string `json:"reason"`
	NextTry  int64  `json:"next_try"`
}

// UnmarshalJSON also reads the bare reason string an earlier build of
// agent-message-queue-611.58 stored (Pro review of #971 round 2): that is
// one refused attempt, due now, so one old record never stops a scan.
func (r *Refusal) UnmarshalJSON(raw []byte) error {
	var reason string
	if json.Unmarshal(raw, &reason) == nil {
		*r = Refusal{Attempts: 1, Reason: reason}
		return nil
	}
	type wire Refusal
	return json.Unmarshal(raw, (*wire)(r))
}

// Retry backoff after a refusal: refuseBackoff, doubling per attempt, at
// most maxRefuseBackoff.
const (
	refuseBackoff    = 30 * time.Second
	maxRefuseBackoff = 10 * time.Minute
)

// Due reports whether the output may be sent at now.
func (o Outbound) Due(now time.Time) bool {
	return o.Refused == nil || now.Unix() >= o.Refused.NextTry
}

// MarkAccepted records the relay's matching positive OK for key.
func (l *Ledger) MarkAccepted(key string, at time.Time) error {
	return l.updateOwed(key, func(o *Outbound) { o.Accepted, o.AcceptedAt = true, at.Unix() })
}

// maxRefusedReason bounds the relay reason kept on a refused output.
const maxRefusedReason = 512

// MarkRefused records a negative OK for key at now: the output stays owed
// and its next attempt waits out the backoff.
func (l *Ledger) MarkRefused(key, reason string, now time.Time) error {
	if len(reason) > maxRefusedReason {
		reason = strings.ToValidUTF8(reason[:maxRefusedReason], "") + "…"
	}
	return l.updateOwed(key, func(o *Outbound) {
		r := Refusal{Reason: reason}
		if o.Refused != nil {
			r.Attempts = o.Refused.Attempts
		}
		r.Attempts++
		wait := refuseBackoff
		for i := 1; i < r.Attempts && wait < maxRefuseBackoff; i++ {
			wait *= 2
		}
		r.NextTry = now.Add(min(wait, maxRefuseBackoff)).Unix()
		o.Refused = &r
	})
}

// updateOwed changes key's record while it is owed; an accepted output
// never changes again.
func (l *Ledger) updateOwed(key string, set func(*Outbound)) error {
	dir := filepath.Join(l.dir, "outbox")
	stored, err := readBounded(filepath.Join(dir, keyFile(key)))
	if err != nil {
		return err
	}
	o, err := readOutbound(stored)
	if err != nil {
		return err
	}
	if o.Accepted {
		return nil
	}
	set(&o)
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(dir, keyFile(key), raw, 0o600)
	return err
}

// Pending returns prepared outputs not yet accepted, in preparation order.
func (l *Ledger) Pending() ([]Outbound, error) {
	return scanOutbox(filepath.Join(l.dir, "outbox"), func(o Outbound) bool { return !o.Accepted })
}

// RefusedOutputs lists the owed outputs the relay refused on their last
// attempt in the ledger under stateDir, in preparation order, without
// creating anything; a ledger that does not exist has none.
func RefusedOutputs(stateDir string) ([]Outbound, error) {
	out, err := scanOutbox(filepath.Join(stateDir, "buzz", "outbox"), func(o Outbound) bool { return !o.Accepted && o.Refused != nil })
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

// scanOutbox returns the records in dir that keep selects, in preparation
// order: records from before sequences first, in key order, then by Seq.
func scanOutbox(dir string, keep func(Outbound) bool) ([]Outbound, error) {
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
		o, err := readOutbound(raw)
		if err != nil {
			return nil, err
		}
		if keep(o) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Key < out[j].Key
	})
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
	o, err := readOutbound(raw)
	return o, err == nil, err
}

func readOutbound(raw []byte) (Outbound, error) {
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

// Approval is one approval message this edge posted: the pending
// interaction it shows and the request fields an answer must carry.
type Approval struct {
	RequestRef    string `json:"request_ref"`
	InteractionID string `json:"interaction_id"`
	Target        string `json:"target"`
	Epoch         string `json:"epoch"`
	Prompt        string `json:"prompt"`
	ApproveOption string `json:"approve_option,omitempty"`
	RejectOption  string `json:"reject_option,omitempty"`
	// Disabled means the message now shows that a remote answer cannot
	// apply; a reaction answers nothing. EditedAt is the second its latest
	// edit is dated, so the next edit is dated strictly later.
	Disabled bool  `json:"disabled,omitempty"`
	EditedAt int64 `json:"edited_at,omitempty"`
	// Pending is an edit recorded but not yet finished: its view, outbox
	// key and date. The carrier finishes it before any other change.
	Pending *ApprovalEdit `json:"pending,omitempty"`
}

// ApprovalEdit is one intended edit of an approval message.
type ApprovalEdit struct {
	View Approval `json:"view"`
	Key  string   `json:"key"`
	At   int64    `json:"at"`
}

// PutApproval maps an approval message's event id to what it shows, so an
// owner reaction on that message answers exactly that interaction.
func (l *Ledger) PutApproval(eventID string, a Approval) error {
	if !validHexID(eventID) {
		return fmt.Errorf("approval event id %q is not a hex id", eventID)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, _, err = createOnce(filepath.Join(l.dir, "receipts"), keyFile("approval/"+eventID), raw)
	return err
}

// UpdateApproval replaces what an approval message shows after an edit.
func (l *Ledger) UpdateApproval(eventID string, a Approval) error {
	if !validHexID(eventID) {
		return fmt.Errorf("approval event id %q is not a hex id", eventID)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(filepath.Join(l.dir, "receipts"), keyFile("approval/"+eventID), raw, 0o600)
	return err
}

// ApprovalFor returns the approval shown by the message with this event id.
func (l *Ledger) ApprovalFor(eventID string) (Approval, bool, error) {
	raw, err := readBounded(filepath.Join(l.dir, "receipts", keyFile("approval/"+eventID)))
	if errors.Is(err, os.ErrNotExist) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, err
	}
	var a Approval
	if err := json.Unmarshal(raw, &a); err != nil {
		return Approval{}, false, fmt.Errorf("approval record is unreadable: %w", err)
	}
	return a, true, nil
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
