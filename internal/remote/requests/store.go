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
// Close releases it.
type Store struct {
	dir      string
	lock     *ownerLock
	now      func() time.Time
	readOnly bool
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

// Close releases the owner lock. The records stay on disk.
func (s *Store) Close() error {
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

// Get reads one record. The boolean is false when no record exists.
func (s *Store) Get(k Key) (*Record, bool, error) {
	p, err := s.path(k)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read record: %w", err)
	}
	rec, err := decodeRecord(data)
	if err != nil {
		return nil, false, fmt.Errorf("record %s: %w", filepath.Base(p), err)
	}
	return rec, true, nil
}

// Create writes revision 1 of a new record. It refuses if a record exists.
func (s *Store) Create(rec *Record) error {
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
	if rec.PublishedRevision < prev.PublishedRevision || rec.PublishedRevision > rec.Revision {
		return protocol.Refuse(protocol.CodeInvalid, "published_revision must be monotonic and not ahead of revision")
	}
	return s.write(rec)
}

// MarkPublished records that revision left through the publisher. It rewrites
// the record in place without a revision bump: publication bookkeeping is not
// new evidence about the request.
func (s *Store) MarkPublished(k Key, revision int64) error {
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

// List returns every record, oldest key first, for reconciliation.
func (s *Store) List() ([]*Record, error) {
	var out []*Record
	base := filepath.Join(s.dir, requestsDir)
	hosts, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(base, h.Name()))
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", h.Name(), err)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), recordSuffix) || strings.HasPrefix(f.Name(), ".") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(base, h.Name(), f.Name()))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", f.Name(), err)
			}
			rec, err := decodeRecord(data)
			if err != nil {
				return nil, fmt.Errorf("record %s: %w", f.Name(), err)
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
	return out, nil
}

// Compact replaces the result and input of terminal records whose evidence
// is older than before with a deduplication tombstone. The request identity,
// digest, epoch and disposition survive, so a replay answers result_expired
// instead of dispatching again.
func (s *Store) Compact(before time.Time) (int, error) {
	recs, err := s.List()
	if err != nil {
		return 0, err
	}
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

func (s *Store) write(rec *Record) error {
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
	}
	return false
}
