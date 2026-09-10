// Package requests is the single-writer durable store for remote request
// records. One record is one atomic JSON file; every change is a new revision
// written with the repository's tmp, fsync, rename primitives. The store
// enforces the request state graph and revision monotonicity; it does not
// dispatch, publish, or talk to a harness.
package requests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Layout under the companion state directory.
const (
	layoutVersion = "v1"
	requestsDir   = "requests"
	ownerLockFile = "owner.lock"
	recordSuffix  = ".json"
	fileMode      = 0o600
	dirMode       = 0o700
)

// MaxRecordBytes bounds one record on disk. It delegates to the protocol
// budget so the store, the wire bound, and the result/input limits are one
// coherent set: a record always holds a full-size result plus input and
// overhead (see protocol.MaxRecordBytes). It is a var, not a const, only so
// tests can shrink it; production never reassigns it.
var MaxRecordBytes = protocol.MaxRecordBytes

var hostSegmentRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// Record is the durable request record: the public snapshot plus the
// bookkeeping only the endpoint reads.
type Record struct {
	protocol.Snapshot

	// Input is the exact submit input, kept until compaction so recovery can
	// show what was asked. It is never re-dispatched from here.
	Input *protocol.SubmitInput `json:"input,omitempty"`

	// PublishedRevision is the last revision known to have left through AMQ.
	PublishedRevision int64 `json:"published_revision"`

	// NativeDispatches counts native submit attempts. The design allows at
	// most one; the store refuses a second.
	NativeDispatches int `json:"native_dispatches"`

	// Answered is the interaction ids this endpoint has already delivered an
	// answer for, with the option sent. It is written before the native call
	// so a replay after a crash cannot answer the same interaction twice.
	Answered map[string]string `json:"answered,omitempty"`

	// AckDigest is the evidence digest of the last acknowledgement sent to the
	// native attachment for a terminal record, written BEFORE the native
	// AcknowledgeResult call. It is the digest of the retained evidence being
	// released (protocol.EvidenceDigest of the bound result), never the input
	// digest, so a replay after a crash can re-send exactly the matching ack
	// and the attachment can refuse one that names different evidence.
	// Empty on non-terminal records and on terminal records whose outcome
	// retained no evidence (nothing to release).
	AckDigest string `json:"ack_digest,omitempty"`

	// Tombstone marks a record that exists only to block a later submit or
	// to remember a compacted result.
	Tombstone bool `json:"tombstone,omitempty"`

	// Origin is carrier-specific routing for publication (for example the
	// AMQ handle and thread the command arrived in). It is never authority.
	Origin map[string]string `json:"origin,omitempty"`

	// UpdatedAt is the store write time, distinct from ObservedAt.
	UpdatedAt string `json:"updated_at"`
}

// Key identifies one record. The creator host is the authenticated source of
// the command, never a claimed handle.
type Key struct {
	CreatorHost string
	TargetID    string
	RequestID   string
}

// Store is the single-writer record store. Open acquires the owner lock;
// Close releases it and marks the store closed so every subsequent mutation
// refuses. A closed store is not writable even if a stale reference survives
// after a replacement endpoint has taken ownership.
type Store struct {
	dir      string
	lock     *ownerLock
	now      func() time.Time
	readOnly bool

	// wmu serializes read-modify-write cycles (Update, MarkPublished,
	// Compact, WriteMemo): the validate step must see the same on-disk
	// revision the write overwrites, or a concurrent compaction/memo can be
	// clobbered by a stale in-memory copy (Pro B14 recut #9 made this window
	// real by moving publication onto an async worker that races operator
	// compaction).
	wmu    sync.Mutex
	closed atomic.Bool
}

// Option configures Open.
type Option func(*Store)

// WithClock injects the clock used for UpdatedAt. Tests pass a fixed clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Open prepares <stateDir>/v1 and takes the owner lock. A second opener on the
// same directory is refused with endpoint_already_running before it can read
// or write any record.
func Open(stateDir string, opts ...Option) (*Store, error) {
	if stateDir == "" {
		return nil, protocol.Refuse(protocol.CodeInvalid, "state directory is required")
	}
	dir := filepath.Join(stateDir, layoutVersion)
	if err := os.MkdirAll(filepath.Join(dir, requestsDir), dirMode); err != nil {
		return nil, fmt.Errorf("create request store: %w", err)
	}
	lock, err := acquireOwnerLock(filepath.Join(dir, ownerLockFile))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: lock, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// OpenReadOnly returns a store that reads records without taking the owner
// lock. Writes through it are refused; the CLI uses it for `requests`.
func OpenReadOnly(stateDir string) (*Store, error) {
	if stateDir == "" {
		return nil, protocol.Refuse(protocol.CodeInvalid, "state directory is required")
	}
	dir := filepath.Join(stateDir, layoutVersion)
	if _, err := os.Stat(filepath.Join(dir, requestsDir)); err != nil {
		return nil, protocol.Refuse(protocol.CodeNotFound, "no request store at %s", dir)
	}
	return &Store{dir: dir, now: time.Now, readOnly: true}, nil
}

// Close releases the owner lock and marks the store closed. Every mutation
// after Close refuses with store_closed, so a stale reference cannot write
// once ownership has moved on. The records stay on disk.
func (s *Store) Close() error {
	s.closed.Store(true)
	if s.lock == nil {
		return nil
	}
	err := s.lock.release()
	s.lock = nil
	return err
}

// Dir is the versioned store directory.
func (s *Store) Dir() string { return s.dir }

// Digest returns the protocol digest of exact submit input bytes.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Store) path(k Key) (string, error) {
	if !safeSegment(k.CreatorHost) || !safeSegment(k.TargetID) {
		return "", protocol.Refuse(protocol.CodeInvalid, "record key has an invalid segment")
	}
	if _, _, _, err := protocol.DecodeRef(protocol.EncodeRef(k.CreatorHost, k.TargetID, k.RequestID)); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, requestsDir, k.CreatorHost, k.TargetID+"__"+k.RequestID+recordSuffix), nil
}

// safeSegment reports whether a key segment is a safe single path component.
// The dot segments are refused here rather than trusting every carrier to
// prefix its host string.
func safeSegment(seg string) bool {
	if seg == "." || seg == ".." {
		return false
	}
	return hostSegmentRe.MatchString(seg)
}

// Get reads one record. The boolean is false when no record exists. It uses
// the same read+normalize path as List so a live request and a recovery pass
// never disagree about a record's state. A present-but-undecodable file is
// returned as an error (poison), not as absent, so a caller does not create a
// conflicting revision-1 over a corrupt file.
func (s *Store) Get(k Key) (*Record, bool, error) {
	p, err := s.path(k)
	if err != nil {
		return nil, false, err
	}
	rec, exists, err := s.readRecord(p)
	if err != nil {
		return nil, false, err
	}
	return rec, exists, nil
}

// Create writes revision 1 of a new record. It refuses if a record exists.
func (s *Store) Create(rec *Record) error {
	if err := s.checkClosed(); err != nil {
		return err
	}
	if rec.Revision != 1 {
		return protocol.Refuse(protocol.CodeInvalid, "new record must be revision 1")
	}
	k := keyOf(rec)
	if _, exists, err := s.Get(k); err != nil {
		return err
	} else if exists {
		return protocol.Refuse(protocol.CodeRequestConflict, "record already exists")
	}
	switch rec.State {
	case protocol.StateReceived, protocol.StateRejected, protocol.StateCancelled:
	default:
		return protocol.Refuse(protocol.CodeInvalid, "a new record starts as received, rejected, or cancelled")
	}
	return s.write(rec)
}

// Update writes the next revision of an existing record after checking the
// state graph, the immutable fields, and the single-dispatch rule.
func (s *Store) Update(rec *Record) error {
	if err := s.checkClosed(); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	prev, exists, err := s.Get(keyOf(rec))
	if err != nil {
		return err
	}
	if !exists {
		return protocol.Refuse(protocol.CodeNotFound, "record does not exist")
	}
	if rec.Revision != prev.Revision+1 {
		return protocol.Refuse(protocol.CodeInvalid, "revision must be %d", prev.Revision+1)
	}
	if err := checkImmutable(prev, rec); err != nil {
		return err
	}
	if !allowed(prev.State, rec.State) {
		return protocol.Refuse(protocol.CodeInvalid, "state %s cannot become %s", prev.State, rec.State)
	}
	if rec.NativeDispatches > 1 || rec.NativeDispatches < prev.NativeDispatches {
		return protocol.Refuse(protocol.CodeInvalid, "native dispatch count must be monotonic and at most 1")
	}
	if prev.State == protocol.StateRejected && rec.State == protocol.StateDispatching && prev.NativeDispatches != 0 {
		// Only a never-dispatched busy-rejected tombstone may be re-admitted
		// by an identical resubmit (Pro B14 recut #3); a dispatch-backed
		// rejection is final.
		return protocol.Refuse(protocol.CodeInvalid, "a dispatched rejection is final; retry with a new request id")
	}
	if rec.PublishedRevision < prev.PublishedRevision || rec.PublishedRevision > rec.Revision {
		return protocol.Refuse(protocol.CodeInvalid, "published_revision must be monotonic and not ahead of revision")
	}
	return s.write(rec)
}

// MarkPublished records that revision left through the publisher. It rewrites
// the record in place without a revision bump: publication bookkeeping is not
// new evidence about the request.
func (s *Store) MarkPublished(k Key, revision int64) error {
	if err := s.checkClosed(); err != nil {
		return err
	}
	// wmu spans the Get→write cycle: without it a concurrent Compact (or
	// another MarkPublished) can advance the on-disk record between the
	// fresh read and the rewrite, and this function's stale copy would
	// clobber it (recut #9 race).
	s.wmu.Lock()
	defer s.wmu.Unlock()
	rec, exists, err := s.Get(k)
	if err != nil {
		return err
	}
	if !exists {
		return protocol.Refuse(protocol.CodeNotFound, "record does not exist")
	}
	if revision > rec.Revision || revision < rec.PublishedRevision {
		return protocol.Refuse(protocol.CodeInvalid, "published revision %d is out of range", revision)
	}
	rec.PublishedRevision = revision
	return s.write(rec)
}

// writeMemo rewrites one record in place without a revision bump or a state
// check, after the caller has applied a bookkeeping-only mutation (ack intent).
// It is the durable-write half of acknowledgement replay: the memo lands
// before the native AcknowledgeResult call so a crash in between leaves a
// record Reconcile can replay the ack from. It validates nothing about the
// record beyond what write already enforces; callers must set the memo field
// on a freshly read record, never on a stale snapshot.
func (s *Store) WriteMemo(rec *Record) error {
	if err := s.checkClosed(); err != nil {
		return err
	}
	// wmu + fresh read: apply ONLY the caller's bookkeeping (ack digest,
	// published revision) onto the current on-disk record. Writing the
	// caller's whole object unconditionally clobbers any concurrent advance
	// (Compact) that happened after the caller read it (recut #9 race).
	s.wmu.Lock()
	defer s.wmu.Unlock()
	cur, exists, err := s.Get(keyOf(rec))
	if err != nil {
		return err
	}
	if !exists {
		return protocol.Refuse(protocol.CodeNotFound, "record does not exist")
	}
	if rec.AckDigest != "" {
		cur.AckDigest = rec.AckDigest
	}
	if rec.PublishedRevision > cur.PublishedRevision {
		cur.PublishedRevision = rec.PublishedRevision
	}
	return s.write(cur)
}

// Poison is one undecodable record encountered during List. The key is
// recovered from the file path so the dedup identity survives even when the
// JSON does not: a later submit for the same request is still blocked by the
// on-disk file, and an operator can see which record to repair. List never
// aborts on a poison record; it isolates it here and continues.
type Poison struct {
	Key   Key
	Path  string
	Error string
}

// List returns every decodable record, oldest key first, for reconciliation.
// A poison (undecodable or invalid) record is isolated and skipped, never
// allowed to abort the whole pass: Reconcile must reach every healthy record
// even when one is corrupt. Use ListWithPoison to also collect diagnostics.
func (s *Store) List() ([]*Record, error) {
	recs, _, err := s.ListWithPoison()
	return recs, err
}

// ListWithPoison returns every decodable record plus a diagnosed poison list.
// Read and decode errors are isolated per file: a bad record is skipped, its
// identity is recovered from the path, and the pass continues. The first
// filesystem error that is not per-record (a host directory that cannot be
// listed) is still returned, because that affects more than one record.
func (s *Store) ListWithPoison() ([]*Record, []Poison, error) {
	var out []*Record
	var poison []Poison
	base := filepath.Join(s.dir, requestsDir)
	hosts, err := os.ReadDir(base)
	if err != nil {
		return nil, nil, fmt.Errorf("list hosts: %w", err)
	}
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		if !safeSegment(h.Name()) {
			// A host directory that is not a valid segment cannot hold records
			// we could round-trip; skip it without aborting the pass.
			continue
		}
		hostDir := filepath.Join(base, h.Name())
		files, err := os.ReadDir(hostDir)
		if err != nil {
			// A directory listing failure is broader than one record; surface it.
			return nil, nil, fmt.Errorf("list %s: %w", h.Name(), err)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), recordSuffix) || strings.HasPrefix(f.Name(), ".") {
				continue
			}
			path := filepath.Join(hostDir, f.Name())
			rec, exists, perr := s.readRecord(path)
			if perr != nil {
				poison = append(poison, Poison{
					Key:   keyFromPath(h.Name(), f.Name()),
					Path:  path,
					Error: perr.Error(),
				})
				continue
			}
			if !exists {
				// Race: the file vanished between ReadDir and ReadFile. Skip it;
				// it is not poison and it is not a record.
				continue
			}
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt < out[j].UpdatedAt
		}
		return out[i].RequestRef < out[j].RequestRef
	})
	return out, poison, nil
}

// readRecord reads and decodes one record file, applying the shared
// state-normalization that both the event (Get) and recovery (List) paths
// use, so a record recovered during Reconcile is never silently stricter or
// looser than one read by a live request. The boolean is false only when the
// file does not exist; a present-but-undecodable file returns an error so a
// caller never mistakes poison for absence.
func (s *Store) readRecord(path string) (*Record, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	rec, err := decodeRecord(data)
	if err != nil {
		return nil, true, fmt.Errorf("record %s: %w", filepath.Base(path), err)
	}
	normalizeRecord(rec)
	return rec, true, nil
}

// keyFromPath recovers a record key from its on-disk path segments so a poison
// record's dedup identity survives even when the JSON does not. The layout is
// <creatorHost>/<targetID>__<requestID>.json. A malformed name yields an empty
// key; the poison entry is still reported with the raw path for repair.
func keyFromPath(host, filename string) Key {
	name := strings.TrimSuffix(filename, recordSuffix)
	idx := strings.Index(name, "__")
	if idx < 0 {
		return Key{CreatorHost: host}
	}
	targetID, requestID := name[:idx], name[idx+2:]
	if !safeSegment(targetID) || requestID == "" {
		return Key{CreatorHost: host}
	}
	return Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
}

// normalizeRecord is the single state-normalization shared by the event and
// recovery read paths. It clamps a decoded record's fields to the invariants
// the store guarantees on write, so Reconcile never acts on a looser reading
// than a live request would. It does not mutate identity (request_ref,
// request_id, creator_host, target_id, epoch, input_digest) or revision, which
// are immutable and already validated by decodeRecord.
func normalizeRecord(rec *Record) {
	if rec == nil {
		return
	}
	if rec.Schema == "" {
		rec.Schema = protocol.SchemaRequest
	}
	if rec.RequestRef == "" {
		rec.RequestRef = protocol.EncodeRef(rec.CreatorHost, rec.TargetID, rec.RequestID)
	}
	// A terminal record must not carry a pending interaction; clear a stale
	// one left by a crash between setting and clearing it.
	if rec.State.Terminal() {
		rec.Interaction = nil
	}
}

// Compact replaces the result and input of terminal records whose evidence
// is older than before with a deduplication tombstone. The request identity,
// digest, epoch and disposition survive, so a replay answers result_expired
// instead of dispatching again.
func (s *Store) Compact(before time.Time) (int, error) {
	if err := s.checkClosed(); err != nil {
		return 0, err
	}
	recs, err := s.List()
	if err != nil {
		return 0, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	n := 0
	for _, rec := range recs {
		if !rec.State.Terminal() || rec.Tombstone {
			continue
		}
		observed, err := protocol.ParseTime(rec.ObservedAt)
		if err != nil || !observed.Before(before) {
			continue
		}
		rec.Revision++
		rec.Result = nil
		rec.Input = nil
		rec.Interaction = nil
		rec.Tombstone = true
		if rec.State == protocol.StateCompleted {
			rec.Code = protocol.CodeResultExpired
		}
		if err := s.write(rec); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// checkClosed refuses every mutation on a closed store. Close sets closed
// before releasing the owner lock, so a stale reference cannot write after a
// replacement endpoint has taken ownership. Reads (Get/List) are still
// permitted on a closed store for diagnosis.
func (s *Store) checkClosed() error {
	if s.closed.Load() {
		return protocol.Refuse(protocol.CodeStoreClosed, "store is closed")
	}
	return nil
}

func (s *Store) write(rec *Record) error {
	if s.closed.Load() {
		return protocol.Refuse(protocol.CodeStoreClosed, "store is closed")
	}
	if s.readOnly {
		return protocol.Refuse(protocol.CodeUnsupported, "store opened read-only")
	}
	if rec.Schema == "" {
		rec.Schema = protocol.SchemaRequest
	}
	if rec.Schema != protocol.SchemaRequest {
		return protocol.Refuse(protocol.CodeInvalid, "record schema must be %s", protocol.SchemaRequest)
	}
	if rec.RequestRef == "" {
		rec.RequestRef = protocol.EncodeRef(rec.CreatorHost, rec.TargetID, rec.RequestID)
	}
	if rec.ObservedAt == "" {
		rec.ObservedAt = protocol.FormatTime(s.now())
	}
	rec.UpdatedAt = protocol.FormatTime(s.now())
	p, err := s.path(keyOf(rec))
	if err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return protocol.Refuse(protocol.CodeStorageFull, "record exceeds %d bytes", MaxRecordBytes)
	}
	if _, err := fsq.WriteFileAtomic(filepath.Dir(p), filepath.Base(p), data, fileMode); err != nil {
		if errors.Is(err, os.ErrPermission) || isNoSpace(err) {
			return protocol.Refuse(protocol.CodeStorageFull, "cannot persist record: %v", err)
		}
		return fmt.Errorf("persist record: %w", err)
	}
	return nil
}

func decodeRecord(data []byte) (*Record, error) {
	if len(data) > MaxRecordBytes {
		return nil, fmt.Errorf("record exceeds %d bytes", MaxRecordBytes)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.Schema != protocol.SchemaRequest || rec.Revision < 1 || rec.RequestID == "" {
		return nil, errors.New("record is not a valid request snapshot")
	}
	return &rec, nil
}

func keyOf(rec *Record) Key {
	return Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
}

func checkImmutable(prev, next *Record) error {
	switch {
	case prev.RequestID != next.RequestID, prev.CreatorHost != next.CreatorHost,
		prev.TargetID != next.TargetID, prev.Epoch != next.Epoch:
		return protocol.Refuse(protocol.CodeInvalid, "record identity is immutable")
	case prev.InputDigest != "" && next.InputDigest != prev.InputDigest:
		return protocol.Refuse(protocol.CodeRequestConflict, "input digest is immutable")
	}
	return nil
}

// allowed is the request state graph from the remote-control ADR. Terminal
// states never change; uncertain may resolve on exact native evidence.
func allowed(from, to protocol.State) bool {
	if from == to {
		return true
	}
	switch from {
	case protocol.StateReceived:
		return to == protocol.StateDispatching || to == protocol.StateRejected || to == protocol.StateCancelled
	case protocol.StateDispatching:
		switch to {
		case protocol.StateRunning, protocol.StateCompleted, protocol.StateFailed,
			protocol.StateRejected, protocol.StateCancelled, protocol.StateUncertain:
			return true
		}
	case protocol.StateRunning:
		switch to {
		case protocol.StateCompleted, protocol.StateFailed, protocol.StateCancelled,
			protocol.StateRejected, protocol.StateUncertain:
			return true
		}
	case protocol.StateUncertain:
		// Exact native evidence resolves an uncertain record to any outcome,
		// including a positive refusal that admission never happened.
		switch to {
		case protocol.StateRunning, protocol.StateCompleted, protocol.StateFailed,
			protocol.StateCancelled, protocol.StateRejected:
			return true
		}
	case protocol.StateRejected:
		// A busy-rejected tombstone (Pro B14 recut #3) may be re-admitted by
		// an identical resubmit: the caller was told the request was refused,
		// and the retry is a NEW admission decision, not a resurrection of a
		// dispatched request. Only reachable for records with
		// NativeDispatches == 0 (never dispatched) — a dispatch-backed
		// rejection is final and the caller retries with a NEW request id.
		if to == protocol.StateDispatching {
			return true
		}
	}
	return false
}
