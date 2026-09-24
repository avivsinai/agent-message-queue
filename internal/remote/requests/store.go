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
	"io"
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

	// RefusalReason is the adapter's refusal text for a rejected record.
	// It stays on the durable record and is never copied onto Snapshot, so a
	// published revision stays byte-identical. Replies surface it as
	// Outcome.Message.
	RefusalReason string `json:"refusal_reason,omitempty"`

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

	// Acknowledged records that the native AcknowledgeResult call for this
	// record DELIVERED (returned without error), written AFTER the call —
	// the durable half of ack replay convergence. AckDigest alone means
	// "intent memoed"; Acknowledged means "delivered". Bookkeeping-only,
	// written in place with no revision bump (mirrors PublishedRevision),
	// so replayTerminalAck short-circuits already-acked terminal records
	// instead of re-Lookuping them every tick forever (agent-message-queue-
	// 611.22.34). Zero-value false for pre-upgrade stores: the first
	// post-upgrade Reconcile replays each terminal record once (Lookup
	// reports EvidenceNone after #744's release-on-ack, the replay returns
	// nil, MarkAcknowledged fires) and then converges permanently.
	Acknowledged bool `json:"acknowledged,omitempty"`

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
	dir  string
	lock *ownerLock
	now  func() time.Time
	// closed is atomic: Close may run on any goroutine (endpoint shutdown)
	// while bookkeeping writes such as MarkAcknowledged (611.22.34) may run
	// on an async native-event goroutine. A plain bool tore under -race.
	closed atomic.Bool
	// mu serializes every durable mutation (Create/Update/WriteMemo/
	// writeMarker). Before 611.22.34 the endpoint's e.mu serialized all
	// writers; the async ack markers broke that assumption. The B3 lost-
	// update probe (MarkAcknowledged vs MarkPublished, 200/200 losses with
	// raw Get->mutate->write) is the regression for this lock. Revision CAS
	// alone cannot close marker-vs-marker races: markers do not bump the
	// revision, so two same-revision marker writes are indistinguishable
	// under compare-and-retry — only mutual exclusion closes it.
	mu       sync.Mutex
	readOnly bool
	// maxStoreBytes is the aggregate quota across every record in this store
	// (611.22.19 BK4). Zero disables the aggregate quota; production sets it
	// via WithMaxStoreBytes. used is the sum of record file sizes on disk,
	// computed once at Open and maintained on every write/compact. A write
	// that would exceed the quota refuses storage_full BEFORE dispatch; local
	// native harness work never reaches this store, so quota pressure cannot
	// stop it. Tombstones for active epochs are never deleted to make space:
	// the quota refuses rather than evicting dedup identity, and compaction
	// (which shrinks records to tombstones) frees space without losing the
	// identity for an active epoch.
	maxStoreBytes int64
	used          int64
	// reserved (611.22.19 BK4 round-2 B3) is the sum of MaxRecordBytes
	// reservations held for admitted-but-not-yet-terminal records. A real
	// reservation, not a point-in-time check: Reserve increments it and
	// tracks the per-key amount; Create/Update convert or release it when the
	// record reaches a terminal state. The quota invariant is
	// used+reserved <= maxStoreBytes, so a result write for an admitted
	// record is never quota-refused — the reservation already paid for it.
	// Fail closed at the door, never after the work ran.
	reserved     int64
	reservedKeys map[Key]int64
}

// Option configures Open.
type Option func(*Store)

// WithClock injects the clock used for UpdatedAt. Tests pass a fixed clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// MaxStoreBytes returns the aggregate quota in bytes (0 = unbounded).
// Tests use this to assert that openServeStore wired DefaultMaxStoreBytes.
func (s *Store) MaxStoreBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxStoreBytes
}

// WithMaxStoreBytes sets the aggregate quota across every record in this
// store (611.22.19 BK4). Zero disables the aggregate quota. Production sets
// protocol.DefaultMaxStoreBytes; tests shrink it. The quota is enforced on
// every write and reservation: a write that would exceed it refuses
// storage_full before dispatch, while local native work (which does not pass
// through this store) is unaffected. Tombstones for active epochs are never
// deleted to make space.
func WithMaxStoreBytes(n int64) Option {
	return func(s *Store) { s.maxStoreBytes = n }
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
	s := &Store{dir: dir, lock: lock, now: time.Now, reservedKeys: map[Key]int64{}}
	for _, o := range opts {
		o(s)
	}
	// 611.22.19 BK4: seed the aggregate-quota usage from existing records so
	// a restarted companion knows what it already owes before admitting new
	// work. A failure to sum is non-fatal: the quota stays enforced per-write
	// via the size-aware accounting in write; only the seed is approximate.
	if s.maxStoreBytes > 0 {
		// Round-4 fold: one walk sums used AND reseeds reservations for
		// non-terminal records with no result (received, dispatching,
		// running). After a restart, running records hold nothing in memory;
		// without reseeding, fresh submits are admitted into their room and
		// their results are refused after the work ran.
		if used, err := s.sumUsed(true); err == nil {
			s.used = used
		}
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
	return &Store{dir: dir, now: time.Now, readOnly: true, reservedKeys: map[Key]int64{}}, nil
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
	s.mu.Lock()
	defer s.mu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkClosed(); err != nil {
		return err
	}
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
	if prev.State == protocol.StateRejected && rec.State == protocol.StateDispatching {
		// Only a busy tombstone (Code == busy AND Tombstone) that never
		// dispatched may be re-admitted by an identical resubmit. Expired,
		// stale_epoch, unshared, storage_full and dispatch-backed rejections
		// are final; the caller retries with a NEW request id.
		if prev.NativeDispatches != 0 || prev.Code != protocol.CodeBusy || !prev.Tombstone {
			return protocol.Refuse(protocol.CodeInvalid, "only a busy tombstone may be re-admitted")
		}
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
	// 611.22.34 B3: the whole read-mutate-write runs under s.mu. The publish
	// arm runs under e.mu today, but the ack markers now run from async
	// goroutines — a raw Get->mutate->write here could revert a just-written
	// Acknowledged flag. The field-level merge (fresh Get inside the lock,
	// set only PublishedRevision, write) is what makes the update safe; a
	// revision CAS alone cannot (markers do not bump the revision, so two
	// same-revision marker writes are indistinguishable).
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if rec.PublishedRevision == revision {
		return nil // already marked; nothing to write
	}
	rec.PublishedRevision = revision
	return s.writeSettlement(rec)
}

// MarkAcknowledged records that the native AcknowledgeResult call RETURNED
// for this record (the adapter contract: AcknowledgeResult has no return
// value, so "returned" is all the flag can prove — an earlier comment said
// "delivered", which over-claimed). It rewrites the record in place without
// a revision bump: ack bookkeeping is not new evidence about the request
// (mirrors MarkPublished). Callers invoke it only after the call returned,
// and only for records whose ack digest is non-empty
// (agent-message-queue-611.22.34). The write is a revision-CAS (writeMarker):
// a concurrent marker writer or commitLocked can advance the revision
// between Get and write; losing that race REFUSES instead of silently
// reverting the other writer's field, and this caller re-reads and retries.
func (s *Store) MarkAcknowledged(k Key) error {
	if err := s.checkClosed(); err != nil {
		return err
	}
	// 611.22.34 B3: the whole read-mutate-write runs under s.mu, setting
	// ONLY Acknowledged on a FRESH read — see MarkPublished for why a
	// revision CAS alone cannot close marker-vs-marker races.
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, exists, err := s.Get(k)
	if err != nil {
		return err
	}
	if !exists {
		return protocol.Refuse(protocol.CodeNotFound, "record does not exist")
	}
	if rec.Acknowledged {
		return nil // already marked; nothing to write
	}
	rec.Acknowledged = true
	return s.writeSettlement(rec)
}

// writeMemo rewrites one record in place without a revision bump or a state
// check, after the caller has applied a bookkeeping-only mutation (ack intent).
// It is the durable-write half of acknowledgement replay: the memo lands
// before the native AcknowledgeResult call so a crash in between leaves a
// record Reconcile can replay the ack from. It validates nothing about the
// record beyond what write already enforces; callers must set the memo field
// on a freshly read record, never on a stale snapshot.
func (s *Store) WriteMemo(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkClosed(); err != nil {
		return err
	}
	return s.writeSettlement(rec)
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
//
// The read is bounded and no-follow (agent-message-queue-qgc). A FIFO or an
// oversized file is an error for this record, never a block and never a
// missing record, so List can skip it and continue with the other targets.
func (s *Store) readRecord(path string) (*Record, bool, error) {
	data, err := readRegularBounded(path, int64(MaxRecordBytes))
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

// readRegularBounded reads one record through the same fail-closed sequence
// as internal/remote/claude/safeopen.go: the leaf must be a regular file no
// larger than maxBytes, opened no-follow where the platform allows it, and
// still that same regular file after open. A file that grows past maxBytes
// between the size check and the read is refused. os.ErrNotExist passes
// through unwrapped so a vanished file stays "not found".
func readRegularBounded(path string, maxBytes int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (mode %s); refusing", fi.Mode())
	}
	if fi.Size() > maxBytes {
		return nil, fmt.Errorf("%d bytes exceeds %d; refusing", fi.Size(), maxBytes)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|recordNoFollowFlag, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi2, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi2.Mode().IsRegular() || !os.SameFile(fi, fi2) {
		return nil, fmt.Errorf("replaced between lstat and open; refusing")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("larger than %d bytes after read; refusing", maxBytes)
	}
	return raw, nil
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
// Compact reaps terminal, settled, old records into tombstones. It lists
// candidates with NO lock held (safe: store.write commits through
// fsq.WriteFileAtomic, so a concurrent reader sees the old record or the new
// one, never a torn one), then re-reads+re-gates+writes each candidate via
// CompactOne. The caller (the endpoint) holds e.mu per candidate, so a
// concurrent Handle cannot interleave. limit bounds the sweep so e.mu is
// held for one record, never across the full list (Pro B7).
//
// The tombstone is local dedup state, not a caller-visible revision:
// Compact does rec.Revision++ and never publishes, so a receiver never
// learns the record became a tombstone. This is intended — the tombstone
// exists so an identical resubmit is re-admittable, not so a caller sees it.
func (s *Store) Compact(before time.Time, limit int) (int, error) {
	if err := s.checkClosed(); err != nil {
		return 0, err
	}
	recs, err := s.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range recs {
		if n >= limit {
			break
		}
		if !rec.State.Terminal() || rec.Tombstone {
			continue
		}
		observed, err := protocol.ParseTime(rec.ObservedAt)
		if err != nil || !observed.Before(before) {
			continue
		}
		// CompactOne re-reads under the caller's lock and re-gates — the
		// List snapshot may be stale (Pro B5).
		compacted, err := s.CompactOne(Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}, before)
		if err != nil {
			return n, err
		}
		if compacted {
			n++
		}
	}
	return n, nil
}

// CompactOne re-reads a single candidate under s.mu, re-gates
// (terminal + !tombstone + old + !OwesAck), and writes the
// tombstone. Returns true if the record was compacted.
//
// 611.22.19 BK4 round-2 B1: the whole read-mutate-write runs under s.mu.
// The previous doc claimed the caller holds the endpoint mutex (e.mu),
// but e.mu does not serialize CompactOne against the async ack markers
// (endpoint.go calls MarkAcknowledged after e.mu.Unlock on the native-event
// goroutine). A lost += on s.used drifted the quota counter for the life of
// the process. CompactOne now takes s.mu like every other writer.
//
// 611.22.19 BK4 round-2 B2: the tombstone write is a settlement write, not
// new work, so it is quota-exempt (writeSettlement). A terminal record with
// no result GROWS on compaction (tombstone flag + revision bump); refusing
// that growth at quota would deadlock the sweep forever, since key-ordered
// List means one refused record stops every later compaction too.
func (s *Store) CompactOne(key Key, before time.Time) (bool, error) {
	if err := s.checkClosed(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, exists, err := s.Get(key)
	if err != nil || !exists {
		return false, err
	}
	// A2: the gate includes settlement — never reap a record we still owe the
	// runtime (bound run, unacked result).
	// Two obligations gate compaction and they are different: OwesAck is
	// what we owe the RUNTIME (release its retained result); an unpublished
	// revision is what we owe the CALLER. Compaction erases the only retained
	// result, so a revision the caller has not received yet must survive it
	// (Pro r2 #14 / packet 4b, agent-message-queue-611.22.36).
	if !rec.State.Terminal() || rec.Tombstone || rec.OwesAck() || rec.PublishedRevision < rec.Revision {
		return false, nil
	}
	observed, err := protocol.ParseTime(rec.ObservedAt)
	if err != nil || !observed.Before(before) {
		return false, nil
	}
	rec.Revision++
	// 611.22.41: mark the tombstone as published. A compacted revision is
	// not a new publication — the tombstone is the record's final published
	// state (repro before the fix: publication state=completed
	// code=result_expired result=nil republished by the next Reconcile).
	// This assignment is safe only because of the compaction gate above:
	// it refuses PublishedRevision < Revision, so every terminal result has
	// already reached the caller before compaction runs and the assignment
	// cannot bury an unpublished one. On a base without that gate it could.
	// (Replaces main's "Intentional asymmetry" comment, which described the
	// opposite behavior from the code it sat on: with PublishedRevision left
	// behind, the publish arm's PublishedRevision < Revision test fires and
	// republishes the tombstone — the exact flooding that comment claimed
	// the asymmetry avoided.)
	rec.PublishedRevision = rec.Revision
	rec.Result = nil
	rec.Input = nil
	rec.Interaction = nil
	rec.Tombstone = true
	if rec.State == protocol.StateCompleted {
		rec.Code = protocol.CodeResultExpired
	}
	if err := s.writeSettlement(rec); err != nil {
		return false, err
	}
	return true, nil
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
	key := keyOf(rec)
	// 611.22.19 BK4 round-2 B3: a real reservation, not a point-in-time check.
	// A key with a reservation (admitted via Reserve before dispatch) may
	// grow up to MaxRecordBytes without a quota check — the reservation
	// already paid for the accepted work AND its bounded result. A result
	// write for an admitted record is never quota-refused. A key WITHOUT a
	// reservation (a deferred Create that was not preceded by Reserve) is
	// fail-closed: used+reserved+delta must fit, or the write refuses
	// storage_full before the work is admitted. Tombstones for active epochs
	// are never deleted to make space: this is a refuse-before-write gate,
	// not an eviction. Local native harness work does not reach this store.
	if s.maxStoreBytes > 0 {
		prevSize := int64(0)
		if fi, err := os.Stat(p); err == nil {
			prevSize = fi.Size()
		}
		delta := int64(len(data)) - prevSize
		_, hasRes := s.reservedKeys[key]
		if delta > 0 && !hasRes && s.used+s.reserved+delta > s.maxStoreBytes {
			return protocol.Refuse(protocol.CodeStorageFull,
				"store at quota: %d bytes used, %d reserved, %d requested, limit %d",
				s.used, s.reserved, delta, s.maxStoreBytes)
		}
		if _, err := fsq.WriteFileAtomic(filepath.Dir(p), filepath.Base(p), data, fileMode); err != nil {
			if errors.Is(err, os.ErrPermission) || isNoSpace(err) {
				return protocol.Refuse(protocol.CodeStorageFull, "cannot persist record: %v", err)
			}
			return fmt.Errorf("persist record: %w", err)
		}
		// Account for the actual delta only after a successful write. A
		// compaction that shrank the record (prevSize > new) reduces used.
		s.used += delta
		// B3: release the reservation when the record reaches a terminal
		// state — the result has landed, the bounded space is now consumed
		// by the actual record, and the reservation has served its purpose.
		// Non-terminal updates (received->dispatching->running) keep the
		// reservation: the result is still pending.
		if rec.State.Terminal() {
			s.releaseReservationLocked(key)
		}
		return nil
	}
	if _, err := fsq.WriteFileAtomic(filepath.Dir(p), filepath.Base(p), data, fileMode); err != nil {
		if errors.Is(err, os.ErrPermission) || isNoSpace(err) {
			return protocol.Refuse(protocol.CodeStorageFull, "cannot persist record: %v", err)
		}
		return fmt.Errorf("persist record: %w", err)
	}
	return nil
}

// writeSettlement persists a settlement write (CompactOne, MarkPublished,
// MarkAcknowledged, WriteMemo) that is exempt from the aggregate quota
// (611.22.19 BK4 round-2 B2). Settlement writes never admit new work: a
// tombstone grows a terminal record, a marker confirms delivery, a memo
// annotates. Refusing them at quota would deadlock the sweep — a terminal
// record with no result GROWS on compaction, and key-ordered List means one
// refused record stops every later compaction too, so the quota can never
// be released. Settlement writes also never touch the reservation: the
// reservation is released on the terminal Update that precedes compaction.
func (s *Store) writeSettlement(rec *Record) error {
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
	prevSize := int64(0)
	if fi, err := os.Stat(p); err == nil {
		prevSize = fi.Size()
	}
	delta := int64(len(data)) - prevSize
	if _, err := fsq.WriteFileAtomic(filepath.Dir(p), filepath.Base(p), data, fileMode); err != nil {
		if errors.Is(err, os.ErrPermission) || isNoSpace(err) {
			return protocol.Refuse(protocol.CodeStorageFull, "cannot persist record: %v", err)
		}
		return fmt.Errorf("persist record: %w", err)
	}
	// Account for the actual delta after a successful write. Settlement
	// writes are quota-exempt but still keep used honest.
	s.used += delta
	return nil
}

// releaseReservationLocked releases the reservation for key, if any. The
// caller holds s.mu.
func (s *Store) releaseReservationLocked(key Key) {
	if amt, ok := s.reservedKeys[key]; ok {
		s.reserved -= amt
		delete(s.reservedKeys, key)
	}
}

// sumUsed walks the record tree and returns the total bytes of all record
// files. It is the seed for the aggregate-quota accounting at Open
// (611.22.19 BK4); per-write deltas keep it current afterward. Read errors
// on individual files are skipped (a vanished file contributes zero),
// mirroring List's poison isolation.
//
// Round-4 fold: reseedReservations is folded into this walk so Open does ONE
// filepath.Walk + JSON-decode pass, not two. When reseed is true, every
// non-terminal record with no result reserves MaxRecordBytes.
func (s *Store) sumUsed(reseed ...bool) (int64, error) {
	doReseed := len(reseed) > 0 && reseed[0]
	var total int64
	base := filepath.Join(s.dir, requestsDir)
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, recordSuffix) {
			return nil
		}
		total += info.Size()
		if doReseed {
			rec, _, rErr := s.readRecord(path)
			if rErr != nil || rec == nil {
				return nil // skip poison records
			}
			if !rec.State.Terminal() && rec.Result == nil {
				k := Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
				if _, exists := s.reservedKeys[k]; !exists {
					s.reserved += int64(MaxRecordBytes)
					s.reservedKeys[k] = int64(MaxRecordBytes)
				}
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return total, nil
}

// Reserve reserves bytes of store capacity for key before dispatch
// (611.22.19 BK4). This is a REAL reservation, not a point-in-time check
// (round-2 B3): it increments s.reserved and tracks the per-key amount so
// the result write for an admitted record is never quota-refused — the
// reservation already paid for the accepted work AND its bounded result.
// The quota invariant used+reserved <= maxStoreBytes is maintained. A zero
// maxStoreBytes store (quota disabled) always succeeds. Callers hold no
// lock; Reserve takes s.mu. The reservation is released automatically when
// the record reaches a terminal state via write, or explicitly via
// ReleaseReservation if the dispatch is abandoned before a write.
func (s *Store) Reserve(key Key, bytes int64) error {
	if s.maxStoreBytes <= 0 || bytes <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Idempotent: a re-Reserve for an already-reserved key (a retry that
	// re-enters the dispatch path) does not double-count.
	if _, ok := s.reservedKeys[key]; ok {
		return nil
	}
	if s.used+s.reserved+bytes > s.maxStoreBytes {
		return protocol.Refuse(protocol.CodeStorageFull,
			"store at quota: %d bytes used, %d reserved, %d requested, limit %d",
			s.used, s.reserved, bytes, s.maxStoreBytes)
	}
	s.reserved += bytes
	s.reservedKeys[key] = bytes
	return nil
}

// ReleaseReservation releases the reservation for key, if any. The endpoint
// calls it when a dispatch is abandoned before a terminal write (e.g. a
// crash-point refusal or a pre-dispatch rejection after Reserve succeeded).
// A terminal write releases the reservation automatically via write, so this
// is only for the gap between Reserve and the first write that does not land.
func (s *Store) ReleaseReservation(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseReservationLocked(key)
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
		// A busy-rejected tombstone (B14c minimal tombstone semantics; full
		// B2/B11 belongs to B14d) may be re-admitted by an identical
		// resubmit: the caller was told the request was refused, and the
		// retry is a NEW admission decision, not a resurrection of a
		// dispatched request. Only re-admission (rejected -> dispatching) is
		// permitted; the Code/Tombstone identity guard lives in Update, where
		// the full record is available, so expired/stale_epoch/unshared/
		// storage_full rejections are never resurrected even at
		// NativeDispatches == 0.
		if to == protocol.StateDispatching {
			return true
		}
	}
	return false
}

// OwesAck reports whether this record has a result we have not released.
// B9 ruling (agent-message-queue-611.22.35): do not erase a result we have
// not acknowledged. Terminal && Result != nil && AckDigest == "" refuses
// compaction, REGARDLESS of NativeRun. The endpoint's owesAck delegates
// to this — one predicate, one place.
func (r *Record) OwesAck() bool {
	return r.State.Terminal() && r.Result != nil && r.AckDigest == ""
}
