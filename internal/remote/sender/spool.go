// Package sender is the durable outgoing submit spool for amq-remote.
//
// It is a reusable durable sender component, NOT state owned by a running
// `up` process. A caller (the CLI) persists one immutable envelope — the
// exact command/digest, destination, target, epoch and expiry — atomically
// BEFORE the caller returns a `submitted` receipt to its user. After a
// restart, a drainer replays pending envelopes through the same
// Endpoint.Handle path a live submit uses, retrying the same identity and
// bytes. An envelope whose admission window has closed is expired without
// dispatch.
//
// The spool's `submitted` state is the SENDER's record that it has accepted
// the user's intent; it is distinct from the target-side `received`/
// `running` state the request store owns. The two are reconciled by the
// request ref the endpoint returns: once an envelope has been dispatched and
// the endpoint owns the record, the spool entry is settled (and later
// reaped); until then it is pending and survives a crash.
//
// Layout: <stateDir>/sender/<creatorHost>__<requestID>.json, one atomic JSON
// file per envelope, written with the repository's tmp/fsync/rename
// primitive (fsq.WriteFileAtomic). One envelope per request id per creator
// host; a retry with the same identity reconciles instead of duplicating.
package sender

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Layout under the companion state directory.
const (
	spoolDir    = "sender"
	spoolSuffix = ".json"
	fileMode    = 0o600
	dirMode     = 0o700
)

// MaxEnvelopeBytes bounds one spool envelope on disk. It is the wire-bound
// command plus routing bookkeeping; it delegates to the protocol command
// bound so the spool never accepts more than the endpoint will.
var MaxEnvelopeBytes = protocol.MaxCommandBytes + 4*1024 // command + overhead

// Envelope is one durable outgoing submit intent.
//
// The Command is the exact bytes the caller built (already validated and
// digested by the protocol layer); the drainer hands it to Endpoint.Handle
// unchanged on replay. Destination carries the routing the caller resolved
// (the endpoint IPC state dir today; a cross-host courier destination in a
// later wave). Target/Epoch/Expiry are the caller's explicit or
// previously-verified values: the spool never retargets a request to a
// different session or substitutes a fresh epoch — an offline enqueue must
// supply them, and a live enqueue copies them from the inspected session.
type Envelope struct {
	// RequestID is the caller-generated UUID; it is the dedup key with
	// CreatorHost. A retry with the same id reconciles instead of resubmitting.
	RequestID string `json:"request_id"`
	// CreatorHost is the authenticated source host of the command (the CLI
	// uses the local host identity; a cross-host edge uses the verified
	// sender). It is part of the spool key so two callers never share an
	// envelope, mirroring the request store's Key.
	CreatorHost string `json:"creator_host"`
	// TargetID is the opaque runtime target.
	TargetID string `json:"target_id"`
	// Epoch is the registration epoch the caller verified for TargetID.
	Epoch string `json:"epoch"`
	// NotAfter is the RFC-3339 admission deadline. An envelope whose window
	// closed while it waited in the spool is expired without dispatch.
	NotAfter string `json:"not_after"`
	// InputDigest is protocol.CommandDigest of Command, persisted so a replay
	// can prove it is retrying the exact same bytes (and so a concurrent
	// duplicate is detected without re-decoding the command).
	InputDigest string `json:"input_digest"`
	// Command is the exact, protocol-valid submit command to dispatch.
	Command *protocol.Command `json:"command"`
	// Destination is the routing the caller resolved. Today it is the
	// endpoint IPC state dir ("ipc:<stateDir>"); later waves add a courier
	// destination. The drainer dispatches through the carrier this names.
	Destination string `json:"destination"`
	// Origin is carrier-specific routing carried through to publication, the
	// same map the endpoint's Handle stores on the record. It is never
	// authority; it is attribution.
	Origin map[string]string `json:"origin,omitempty"`
	// State is the spool-side state: pending, dispatched, expired, failed.
	State State `json:"state"`
	// CreatedAt is the persist time (the moment the caller got `submitted`).
	CreatedAt string `json:"created_at"`
	// DispatchedAt is set when the drainer handed the command to the
	// endpoint and the endpoint accepted it (returned a non-transient reply).
	DispatchedAt string `json:"dispatched_at,omitempty"`
	// SettledAt is set when the spool entry is reaped (dispatched + settled
	// or expired). Settled entries are reaped on the next sweep.
	SettledAt string `json:"settled_at,omitempty"`
	// Attempt is the dispatch attempt count (retries on transient failure).
	Attempt int `json:"attempt,omitempty"`
	// LastError records the last dispatch failure (cleared on success).
	LastError string `json:"last_error,omitempty"`
}

// State is the spool-side state of an outgoing envelope. It is the SENDER's
// view, distinct from the target-side request state the request store owns.
type State string

const (
	// StatePending: persisted, not yet accepted by the endpoint.
	StatePending State = "pending"
	// StateDispatched: the endpoint accepted the command and owns the record
	// now; the spool entry is settled and will be reaped.
	StateDispatched State = "dispatched"
	// StateExpired: the admission window closed before the endpoint accepted
	// the command; no dispatch happened. Action-required for the caller.
	StateExpired State = "expired"
	// StateFailed: the endpoint refused the command with a terminal (non-
	// transient) refusal; no retry will succeed. The refusal is in LastError.
	StateFailed State = "failed"
)

// Terminal reports whether a spool state is settled (no further dispatch).
func (s State) Terminal() bool {
	switch s {
	case StateDispatched, StateExpired, StateFailed:
		return true
	}
	return false
}

// Spool is the durable outgoing submit spool. It is safe for concurrent use.
// Open does NOT take an owner lock: the spool is append/reap storage shared
// by the CLI writer and the companion drainer, not a single-writer store like
// the request store. Per-file atomicity (fsq.WriteFileAtomic) makes
// concurrent writers safe: two writers for the same key reconcile to one
// file (Create refuses a duplicate; a retry reads the existing envelope).
type Spool struct {
	dir string
	now func() time.Time
	mu  sync.Mutex
}

// Option configures Open.
type Option func(*Spool)

// WithClock injects the clock used for timestamps. Tests pass a fixed clock.
func WithClock(now func() time.Time) Option {
	return func(s *Spool) { s.now = now }
}

var (
	hostSegmentRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	uuidRe        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Open prepares <stateDir>/sender and returns a spool. The directory is
// created if missing; existing envelopes are recovered by List.
func Open(stateDir string, opts ...Option) (*Spool, error) {
	if stateDir == "" {
		return nil, protocol.Refuse(protocol.CodeInvalid, "state directory is required")
	}
	dir := filepath.Join(stateDir, spoolDir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create sender spool: %w", err)
	}
	s := &Spool{dir: dir, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Dir is the spool directory.
func (s *Spool) Dir() string { return s.dir }

// Create persists one envelope atomically BEFORE returning. A duplicate
// (same creator host + request id) reconciles: if the existing envelope has
// the same digest, Create returns it as-is so the CLI proceeds exactly as
// main does (exit 0, existing record). Only a DIFFERENT digest under the
// same id is a conflict (the caller changed the command bytes). The
// envelope starts in StatePending.
//
// The caller MUST have already validated the command (protocol.DecodeCommand
// or protocol.Validate) and resolved target+epoch (from a live inspect or an
// explicit previously-verified value). The spool never retargets: it stores
// exactly what the caller supplied.
func (s *Spool) Create(env *Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateEnvelope(env); err != nil {
		return err
	}
	existing, exists, err := s.get(env.key())
	if err != nil {
		return err
	}
	if exists {
		// B1: a retry with the same identity and the same digest reconciles
		// to the existing envelope — the caller proceeds as main does (exit
		// 0). Only a different digest under the same id is a conflict.
		if existing.InputDigest == env.InputDigest {
			*env = *existing
			return nil
		}
		return protocol.Refuse(protocol.CodeRequestConflict, "request %s already has a different digest", env.RequestID)
	}
	env.State = StatePending
	if env.CreatedAt == "" {
		env.CreatedAt = protocol.FormatTime(s.now())
	}
	return s.write(env)
}

// Get reads one envelope. The boolean is false when no envelope exists.
func (s *Spool) Get(creatorHost, requestID string) (*Envelope, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(Key{CreatorHost: creatorHost, RequestID: requestID})
}

// List returns every envelope, oldest first. Pending entries are returned
// first (so a drainer replays them before reaping settled ones).
func (s *Spool) List() ([]*Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list sender spool: %w", err)
	}
	var out []*Envelope
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), spoolSuffix) || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		env, exists, err := s.read(filepath.Join(s.dir, e.Name()))
		if err != nil {
			// A poison envelope is skipped, not fatal: the drainer must
			// reach every healthy envelope. An operator can inspect the file.
			continue
		}
		if !exists {
			continue
		}
		out = append(out, env)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].RequestID < out[j].RequestID
	})
	// Pending first: a drainer scanning the list dispatches pending entries
	// before reaping settled ones.
	sort.SliceStable(out, func(i, j int) bool {
		pi := out[i].State == StatePending
		pj := out[j].State == StatePending
		if pi != pj {
			return pi
		}
		return false
	})
	return out, nil
}

// MarkDispatched settles an envelope: the endpoint accepted the command and
// owns the record. Settled entries are reaped by Reap.
func (s *Spool) MarkDispatched(k Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	env, exists, err := s.get(k)
	if err != nil {
		return err
	}
	if !exists {
		return protocol.Refuse(protocol.CodeNotFound, "envelope does not exist")
	}
	if env.State.Terminal() {
		return nil // already settled; idempotent
	}
	env.State = StateDispatched
	env.DispatchedAt = protocol.FormatTime(s.now())
	env.SettledAt = env.DispatchedAt // B5: every terminal state stamps settle time.
	env.LastError = ""
	env.Attempt++
	return s.write(env)
}

// MarkFailed records a terminal dispatch failure (a non-transient endpoint
// refusal). The envelope stays for diagnosis and is reaped by Reap.
func (s *Spool) MarkFailed(k Key, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	env, exists, err := s.get(k)
	if err != nil {
		return err
	}
	if !exists {
		return protocol.Refuse(protocol.CodeNotFound, "envelope does not exist")
	}
	if env.State.Terminal() {
		return nil
	}
	env.State = StateFailed
	env.LastError = errMsg
	env.Attempt++
	env.SettledAt = protocol.FormatTime(s.now()) // B5: every terminal state stamps settle time.
	return s.write(env)
}

// MarkAttempt records a transient dispatch failure (the drainer will retry).
// The envelope stays pending; the attempt count and last error are updated.
func (s *Spool) MarkAttempt(k Key, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	env, exists, err := s.get(k)
	if err != nil {
		return err
	}
	if !exists {
		return protocol.Refuse(protocol.CodeNotFound, "envelope does not exist")
	}
	if env.State.Terminal() {
		return nil
	}
	env.Attempt++
	env.LastError = errMsg
	return s.write(env)
}

// Expire marks a pending envelope whose admission window has closed as
// expired WITHOUT dispatching. Returns true if the envelope was expired by
// this call (was pending and past its deadline).
func (s *Spool) Expire(k Key, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	env, exists, err := s.get(k)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, protocol.Refuse(protocol.CodeNotFound, "envelope does not exist")
	}
	if env.State.Terminal() {
		return false, nil
	}
	deadline, err := protocol.ParseTime(env.NotAfter)
	if err != nil {
		return false, err
	}
	if !now.After(deadline) {
		return false, nil
	}
	env.State = StateExpired
	env.SettledAt = protocol.FormatTime(s.now())
	return true, s.write(env)
}

// Reap removes settled envelopes older than before. It bounds the spool's
// growth: dispatched/expired/failed entries are removed once they are old
// enough that the caller has had a chance to observe the outcome. Returns
// the count reaped.
func (s *Spool) Reap(before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("reap sender spool: %w", err)
	}
	n := 0
	for _, e := range entries {
		if n >= limit {
			break
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), spoolSuffix) || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		env, exists, err := s.read(path)
		if err != nil || !exists {
			continue
		}
		if !env.State.Terminal() {
			continue
		}
		settled := env.SettledAt
		if settled == "" {
			settled = env.DispatchedAt
		}
		if settled == "" {
			continue
		}
		t, err := protocol.ParseTime(settled)
		if err != nil || !t.Before(before) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return n, err
		}
		n++
	}
	return n, nil
}

// Key identifies one envelope: creator host + request id.
type Key struct {
	CreatorHost string
	RequestID   string
}

func (e *Envelope) key() Key {
	return Key{CreatorHost: e.CreatorHost, RequestID: e.RequestID}
}

func (s *Spool) filename(k Key) (string, error) {
	if !safeSegment(k.CreatorHost) {
		return "", protocol.Refuse(protocol.CodeInvalid, "creator host is not a safe segment")
	}
	if !uuidRe.MatchString(k.RequestID) {
		return "", protocol.Refuse(protocol.CodeInvalid, "request_id must be a lowercase UUID")
	}
	return filepath.Join(s.dir, k.CreatorHost+"__"+k.RequestID+spoolSuffix), nil
}

func (s *Spool) get(k Key) (*Envelope, bool, error) {
	name, err := s.filename(k)
	if err != nil {
		return nil, false, err
	}
	return s.read(name)
}

func (s *Spool) read(path string) (*Envelope, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("read envelope: %w", err)
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, true, fmt.Errorf("decode envelope: %w", err)
	}
	return &env, true, nil
}

func (s *Spool) write(env *Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	if len(data) > MaxEnvelopeBytes {
		return protocol.Refuse(protocol.CodeStorageFull, "envelope exceeds %d bytes", MaxEnvelopeBytes)
	}
	name, err := s.filename(env.key())
	if err != nil {
		return err
	}
	if _, err := fsq.WriteFileAtomic(filepath.Dir(name), filepath.Base(name), data, fileMode); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return protocol.Refuse(protocol.CodeStorageFull, "cannot persist envelope: %v", err)
		}
		return fmt.Errorf("persist envelope: %w", err)
	}
	return nil
}

func safeSegment(seg string) bool {
	if seg == "." || seg == ".." {
		return false
	}
	return hostSegmentRe.MatchString(seg)
}

func validateEnvelope(env *Envelope) error {
	if env == nil {
		return protocol.Refuse(protocol.CodeInvalid, "envelope is nil")
	}
	if env.Command == nil {
		return protocol.Refuse(protocol.CodeInvalid, "command is required")
	}
	if env.Command.Op != protocol.OpRequestSubmit {
		return protocol.Refuse(protocol.CodeInvalid, "envelope command must be %s", protocol.OpRequestSubmit)
	}
	if !uuidRe.MatchString(env.RequestID) {
		return protocol.Refuse(protocol.CodeInvalid, "request_id must be a lowercase UUID")
	}
	if env.RequestID != env.Command.RequestID {
		return protocol.Refuse(protocol.CodeInvalid, "envelope request_id must match command request_id")
	}
	if !safeSegment(env.CreatorHost) {
		return protocol.Refuse(protocol.CodeInvalid, "creator_host is not a safe segment")
	}
	if env.Command.TargetID == "" || env.Command.Epoch == "" {
		return protocol.Refuse(protocol.CodeInvalid, "command target_id and epoch are required")
	}
	if env.TargetID != env.Command.TargetID || env.Epoch != env.Command.Epoch {
		return protocol.Refuse(protocol.CodeInvalid, "envelope target/epoch must match command")
	}
	if _, err := protocol.ParseTime(env.NotAfter); err != nil {
		return protocol.Refuse(protocol.CodeInvalid, "not_after must be RFC 3339: %v", err)
	}
	if env.Command.NotAfter != env.NotAfter {
		return protocol.Refuse(protocol.CodeInvalid, "envelope not_after must match command not_after")
	}
	// Persist the digest so a replay can prove it retries the exact bytes
	// and so a duplicate is detected without re-decoding. Recompute to verify
	// the caller did not supply a mismatched digest.
	digest := protocol.CommandDigest(env.Command)
	if env.InputDigest != "" && env.InputDigest != digest {
		return protocol.Refuse(protocol.CodeRequestConflict, "input_digest does not match command")
	}
	env.InputDigest = digest
	if env.Destination == "" {
		return protocol.Refuse(protocol.CodeInvalid, "destination is required")
	}
	if err := env.Command.Validate(); err != nil {
		return err
	}
	return nil
}

// SpoolReceipt is the SENDER-side `submitted` receipt returned when the
// endpoint could not be reached (or returned a duplicate). It is NOT a
// Snapshot: it never mints a request ref or a revision the endpoint did not
// assign. The receipt carries the envelope identity, the resolved target/
// epoch/expiry, the destination, and the spool state (pending). If the
// envelope carried a --min-evidence floor, the receipt says it is unevaluated
// until the drainer dispatches (the endpoint evaluates at drain, not at
// persist). This is distinct from the target-side received/running state the
// endpoint owns.
type SpoolReceipt struct {
	Schema      string `json:"schema"`                 // "sender_submitted"
	RequestID   string `json:"request_id"`             // caller-generated UUID
	CreatorHost string `json:"creator_host"`           // authenticated source host
	TargetID    string `json:"target_id"`              // resolved target
	Epoch       string `json:"epoch"`                  // resolved/verified epoch
	NotAfter    string `json:"not_after"`              // admission deadline
	Destination string `json:"destination"`            // resolved routing (ipc:<dir>)
	State       State  `json:"state"`                  // pending (not received/running)
	InputDigest string `json:"input_digest"`           // command digest for dedup
	MinEvidence string `json:"min_evidence,omitempty"` // floor, unevaluated until drain
	Reason      string `json:"reason"`                 // "endpoint_unreachable" or "duplicate_conflict"
	CreatedAt   string `json:"created_at"`             // persist time
	LastError   string `json:"last_error,omitempty"`   // round-3 B6: refusal code for failed envelopes
}

// Receipt builds a SpoolReceipt from an envelope. The state is always pending:
// the intent is durable and the drainer will dispatch it when the companion
// runs. The reason explains why the receipt was issued instead of a live
// reply.
func (env *Envelope) Receipt(reason string) SpoolReceipt {
	r := SpoolReceipt{
		Schema:      "sender_submitted",
		RequestID:   env.RequestID,
		CreatorHost: env.CreatorHost,
		TargetID:    env.TargetID,
		Epoch:       env.Epoch,
		NotAfter:    env.NotAfter,
		Destination: env.Destination,
		State:       env.State,
		InputDigest: env.InputDigest,
		CreatedAt:   env.CreatedAt,
		Reason:      reason,
		LastError:   env.LastError,
	}
	return r
}
