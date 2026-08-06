package registry

import (
	"context"
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
)

const (
	SchemaVersion = 1

	StateAttached State = "attached"
	StateActive   State = "active"
	StateDetached State = "detached"
)

type State string

type Entry struct {
	ID                     string    `json:"id"`
	Root                   string    `json:"root"`
	BaseRoot               string    `json:"base_root,omitempty"`
	SessionName            string    `json:"session_name,omitempty"`
	Agent                  string    `json:"agent"`
	Adapter                string    `json:"adapter"`
	Target                 string    `json:"target"`
	State                  State     `json:"state"`
	LastAttach             time.Time `json:"last_attach,omitempty"`
	LastSeenBySupervisor   time.Time `json:"last_seen_by_supervisor,omitempty"`
	FailureCount           int       `json:"failure_count,omitempty"`
	BackoffUntil           time.Time `json:"backoff_until,omitempty"`
	NextHealthCheck        time.Time `json:"next_health_check,omitempty"`
	DetachedSince          time.Time `json:"detached_since,omitempty"`
	LastError              string    `json:"last_error,omitempty"`
	LastSupervisorDecision string    `json:"last_supervisor_decision,omitempty"`
}

type File struct {
	SchemaVersion int     `json:"schema_version"`
	Entries       []Entry `json:"entries"`
}

type Store struct {
	Path string
	Now  func() time.Time
}

var ErrCorrupt = errors.New("registry file is corrupt")
var ErrTargetOwned = errors.New("adapter target is already owned")
var ErrRegistrationOwned = errors.New("AMQ root and agent already have a wake registration")

type EntryUpdate struct {
	Before Entry
	After  Entry
}

type UpdateResult struct {
	Updated int
	Skipped int
}

var processLocks sync.Map
var registrationLocks sync.Map

type registrationSemaphore struct {
	token chan struct{}
}

func newRegistrationSemaphore() *registrationSemaphore {
	semaphore := &registrationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func New(path string) *Store {
	return &Store{Path: path, Now: time.Now}
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".amq-keepalive", "registry.json"), nil
}

func EntryID(root, agent, adapterName, target string) string {
	sum := sha256.Sum256([]byte(canonicalRoot(root) + "\x00" + agent + "\x00" + adapterName + "\x00" + target))
	return hex.EncodeToString(sum[:])
}

// CanonicalRoot returns the stable filesystem identity used for AMQ wake
// ownership. Existing roots are resolved through symlinks; a not-yet-created
// root still receives an absolute, cleaned spelling so registration remains
// deterministic before AMQ creates its layout.
func CanonicalRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("entry root is required")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

func canonicalRoot(root string) string {
	canonical, err := CanonicalRoot(root)
	if err != nil {
		return filepath.Clean(root)
	}
	return canonical
}

func (s *Store) Load() (File, error) {
	if s.Path == "" {
		return File{}, errors.New("registry path is required")
	}
	var file File
	err := s.withLock(func() error {
		loaded, err := s.loadUnlocked()
		file = loaded
		return err
	})
	return file, err
}

// WithRegistrationLock serializes the complete attach/reattach transaction
// across processes without blocking ordinary registry readers. Callers hold
// this lease from their fresh target inventory and ownership preflight through
// wake readiness and the final registry commit, so a racing claimant cannot
// start a wake before discovering the winner.
func (s *Store) WithRegistrationLock(fn func() error) error {
	return s.WithRegistrationLockContext(context.Background(), fn)
}

func (s *Store) WithRegistrationLockContext(ctx context.Context, fn func() error) error {
	if s.Path == "" {
		return errors.New("registry path is required")
	}
	if fn == nil {
		return errors.New("registration transaction is required")
	}
	path, err := filepath.Abs(s.Path + ".registration.lock")
	if err != nil {
		return err
	}
	semaphoreValue, _ := registrationLocks.LoadOrStore(path, newRegistrationSemaphore())
	semaphore := semaphoreValue.(*registrationSemaphore)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-semaphore.token:
	}
	defer func() { semaphore.token <- struct{}{} }()

	if err := ensureRegistryDir(filepath.Dir(path)); err != nil {
		return err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := lock.Chmod(0o600); err != nil {
		return err
	}
	for {
		acquired, err := flockTryExclusive(lock)
		if err != nil {
			return err
		}
		if acquired {
			break
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer flockRelease(lock)
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func (s *Store) loadUnlocked() (File, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return File{SchemaVersion: SchemaVersion}, nil
	}
	if err != nil {
		return File{}, err
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return File{}, fmt.Errorf("%w %q: %w", ErrCorrupt, s.Path, err)
	}
	if file.SchemaVersion == 0 {
		file.SchemaVersion = SchemaVersion
	}
	if file.SchemaVersion != SchemaVersion {
		return File{}, fmt.Errorf("unsupported registry schema version %d", file.SchemaVersion)
	}
	sortEntries(file.Entries)
	return file, nil
}

func (s *Store) Save(file File) error {
	if s.Path == "" {
		return errors.New("registry path is required")
	}
	return s.withLock(func() error {
		return s.saveUnlocked(file)
	})
}

func (s *Store) saveUnlocked(file File) error {
	file.SchemaVersion = SchemaVersion
	sortEntries(file.Entries)

	dir := filepath.Dir(s.Path)
	tmp, err := os.CreateTemp(dir, ".registry-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(file); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return err
	}
	if err := os.Chmod(s.Path, 0o600); err != nil {
		return err
	}
	return syncDir(dir)
}

func (s *Store) Upsert(entry Entry) (Entry, error) {
	prepared, err := s.prepareEntry(entry)
	if err != nil {
		return Entry{}, err
	}

	err = s.withLock(func() error {
		file, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if owner, ok := conflictingRegistrationOwner(file.Entries, prepared); ok {
			if sameRegistration(owner, prepared) {
				for i := range file.Entries {
					if file.Entries[i].ID == owner.ID {
						file.Entries[i] = prepared
						return s.saveUnlocked(file)
					}
				}
			}
			return registrationOwnedError(prepared, owner)
		}
		if owner, ok := conflictingTargetOwner(file.Entries, prepared, false); ok {
			return targetOwnedError(prepared, owner)
		}
		replaced := false
		for i := range file.Entries {
			if file.Entries[i].ID == prepared.ID {
				file.Entries[i] = prepared
				replaced = true
				break
			}
		}
		if !replaced {
			file.Entries = append(file.Entries, prepared)
		}
		return s.saveUnlocked(file)
	})
	return prepared, err
}

func (s *Store) ReplaceSessionAdapter(entry Entry) (Entry, []Entry, error) {
	prepared, err := s.prepareEntry(entry)
	if err != nil {
		return Entry{}, nil, err
	}

	var removed []Entry
	err = s.withLock(func() error {
		file, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if owner, ok := conflictingTargetOwner(file.Entries, prepared, true); ok {
			return targetOwnedError(prepared, owner)
		}
		next := make([]Entry, 0, len(file.Entries)+1)
		for _, existing := range file.Entries {
			// AMQ permits one wake process per root and agent. Reattach therefore
			// replaces the old registration even when the terminal adapter changed.
			if sameRootAgent(existing, prepared) {
				removed = append(removed, existing)
				continue
			}
			next = append(next, existing)
		}
		next = append(next, prepared)
		file.Entries = next
		return s.saveUnlocked(file)
	})
	return prepared, removed, err
}

// RestoreSessionAdapterIfUnchanged rolls back a pre-wake reattach reservation
// only while that exact inactive row is still authoritative. It restores the
// complete prior root/agent set atomically; a concurrent change wins and keeps
// the recoverable reservation instead of being overwritten.
func (s *Store) RestoreSessionAdapterIfUnchanged(expected Entry, previous []Entry) (bool, error) {
	if expected.ID == "" {
		return false, errors.New("expected reservation id is required")
	}
	restored := false
	err := s.withLock(func() error {
		file, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		index := -1
		for i, entry := range file.Entries {
			if sameRootAgent(entry, expected) && entry.ID != expected.ID {
				return nil
			}
			if entry.ID == expected.ID {
				if entry != expected {
					return nil
				}
				index = i
			}
		}
		if index < 0 {
			return nil
		}
		next := make([]Entry, 0, len(file.Entries)-1+len(previous))
		next = append(next, file.Entries[:index]...)
		next = append(next, file.Entries[index+1:]...)
		next = append(next, previous...)
		file.Entries = next
		if err := s.saveUnlocked(file); err != nil {
			return err
		}
		restored = true
		return nil
	})
	return restored, err
}

// CheckTargetAvailable is the read-only preflight used before a reattach
// touches AMQ. ReplaceSessionAdapter repeats the check under its write lock, so
// a racing owner still fails closed before registry mutation.
func (s *Store) CheckTargetAvailable(entry Entry, ignoreSameRootAgent bool) error {
	prepared, err := s.prepareEntry(entry)
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		file, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if owner, ok := conflictingTargetOwner(file.Entries, prepared, ignoreSameRootAgent); ok {
			return targetOwnedError(prepared, owner)
		}
		return nil
	})
}

func (s *Store) prepareEntry(entry Entry) (Entry, error) {
	now := s.now()
	root, err := CanonicalRoot(entry.Root)
	if err != nil {
		return Entry{}, err
	}
	entry.Root = root
	if entry.Agent == "" {
		return Entry{}, errors.New("entry agent is required")
	}
	if entry.Adapter == "" {
		return Entry{}, errors.New("entry adapter is required")
	}
	if entry.Target == "" {
		return Entry{}, errors.New("entry target is required")
	}
	if entry.ID == "" {
		entry.ID = EntryID(entry.Root, entry.Agent, entry.Adapter, entry.Target)
	}
	if entry.State == "" {
		entry.State = StateAttached
	}
	if entry.LastAttach.IsZero() {
		entry.LastAttach = now
	}
	return entry, nil
}

func (s *Store) UpdateEntry(entry Entry) error {
	return s.withLock(func() error {
		file, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		for i := range file.Entries {
			if file.Entries[i].ID == entry.ID {
				file.Entries[i] = entry
				return s.saveUnlocked(file)
			}
		}
		return fmt.Errorf("registry entry %q not found", entry.ID)
	})
}

// UpdateEntries applies a supervisor pass under one lock and one atomic save.
// Each update is an optimistic compare-and-swap: a concurrent attach,
// reattach, forget, or supervisor pass wins over a stale snapshot instead of
// being overwritten or resurrected.
func (s *Store) UpdateEntries(updates []EntryUpdate) (UpdateResult, error) {
	var result UpdateResult
	seen := make(map[string]struct{}, len(updates))
	for _, update := range updates {
		if update.Before.ID == "" || update.After.ID == "" {
			return result, errors.New("batch update entry id is required")
		}
		if update.Before.ID != update.After.ID {
			return result, fmt.Errorf("batch update changes entry id from %q to %q", update.Before.ID, update.After.ID)
		}
		if _, ok := seen[update.Before.ID]; ok {
			return result, fmt.Errorf("batch update contains duplicate entry %q", update.Before.ID)
		}
		seen[update.Before.ID] = struct{}{}
	}
	if len(updates) == 0 {
		return result, nil
	}

	err := s.withLock(func() error {
		file, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		byID := make(map[string]int, len(file.Entries))
		for i := range file.Entries {
			byID[file.Entries[i].ID] = i
		}
		for _, update := range updates {
			i, ok := byID[update.Before.ID]
			if !ok || file.Entries[i] != update.Before {
				result.Skipped++
				continue
			}
			if update.Before == update.After {
				continue
			}
			file.Entries[i] = update.After
			result.Updated++
		}
		if result.Updated == 0 {
			return nil
		}
		return s.saveUnlocked(file)
	})
	return result, err
}

func (s *Store) Forget(id string) (bool, error) {
	removed, err := s.ForgetMany([]string{id})
	return removed == 1, err
}

// ForgetIfUnchanged removes exactly the entry that was inspected. A concurrent
// reattach or supervisor update causes a safe skip instead of deleting newer
// state.
func (s *Store) ForgetIfUnchanged(expected Entry) (bool, error) {
	if expected.ID == "" {
		return false, errors.New("expected entry id is required")
	}
	removed := false
	err := s.withLock(func() error {
		file, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		for i, entry := range file.Entries {
			if entry.ID != expected.ID {
				continue
			}
			if entry != expected {
				return nil
			}
			file.Entries = append(file.Entries[:i], file.Entries[i+1:]...)
			removed = true
			return s.saveUnlocked(file)
		}
		return nil
	})
	return removed, err
}

func (s *Store) ForgetMany(ids []string) (int, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			wanted[id] = struct{}{}
		}
	}
	removed := 0
	err := s.withLock(func() error {
		file, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		found := 0
		for _, entry := range file.Entries {
			if _, ok := wanted[entry.ID]; ok {
				found++
			}
		}
		if found != len(wanted) {
			return fmt.Errorf("found %d of %d registry entries requested for removal", found, len(wanted))
		}
		next := file.Entries[:0]
		for _, entry := range file.Entries {
			if _, ok := wanted[entry.ID]; ok {
				removed++
				continue
			}
			next = append(next, entry)
		}
		file.Entries = next
		return s.saveUnlocked(file)
	})
	return removed, err
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})
}

func conflictingTargetOwner(entries []Entry, candidate Entry, ignoreSameRootAgent bool) (Entry, bool) {
	candidateTarget := canonicalStoredTarget(candidate.Adapter, candidate.Target)
	for _, existing := range entries {
		if existing.ID == candidate.ID {
			continue
		}
		if ignoreSameRootAgent && sameRootAgent(existing, candidate) {
			continue
		}
		if existing.Adapter == candidate.Adapter && canonicalStoredTarget(existing.Adapter, existing.Target) == candidateTarget {
			return existing, true
		}
	}
	return Entry{}, false
}

func conflictingRegistrationOwner(entries []Entry, candidate Entry) (Entry, bool) {
	for _, existing := range entries {
		if existing.ID == candidate.ID {
			continue
		}
		if sameRootAgent(existing, candidate) {
			return existing, true
		}
	}
	return Entry{}, false
}

func sameRegistration(left, right Entry) bool {
	return sameRootAgent(left, right) &&
		left.Adapter == right.Adapter &&
		canonicalStoredTarget(left.Adapter, left.Target) == canonicalStoredTarget(right.Adapter, right.Target)
}

func sameRootAgent(left, right Entry) bool {
	return left.Agent == right.Agent && canonicalRoot(left.Root) == canonicalRoot(right.Root)
}

func canonicalStoredTarget(adapterName, target string) string {
	target = strings.TrimSpace(target)
	switch adapterName {
	case "cmux", "ghostty":
		// UUIDs are case-insensitive. This also protects new canonical writers
		// from legacy registry rows which persisted lower-case terminal IDs.
		return strings.ToLower(target)
	case "file":
		return canonicalStoredFileTarget(target)
	}
	return target
}

// canonicalStoredFileTarget mirrors the durable identity portion of the file
// adapter's normalization without rejecting legacy rows that can no longer be
// fully resolved. A visible target resolves through symlinks; for a not-yet-
// created file, resolving its parent still prevents alias paths from claiming
// the same future destination independently.
func canonicalStoredFileTarget(target string) string {
	abs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return filepath.Clean(target)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(filepath.Clean(parent), filepath.Base(abs))
	}
	return abs
}

func targetOwnedError(candidate, owner Entry) error {
	return fmt.Errorf(
		"%w: adapter=%q target=%q requested_by=%s@%s existing_owner=%s@%s existing_id=%s",
		ErrTargetOwned,
		candidate.Adapter,
		candidate.Target,
		candidate.Agent,
		candidate.Root,
		owner.Agent,
		owner.Root,
		owner.ID,
	)
}

func registrationOwnedError(candidate, owner Entry) error {
	return fmt.Errorf(
		"%w: requested_adapter=%q requested_target=%q requested_by=%s@%s existing_adapter=%q existing_target=%q existing_id=%s",
		ErrRegistrationOwned,
		candidate.Adapter,
		candidate.Target,
		candidate.Agent,
		candidate.Root,
		owner.Adapter,
		owner.Target,
		owner.ID,
	)
}

func (s *Store) withLock(fn func() error) error {
	if s.Path == "" {
		return errors.New("registry path is required")
	}
	path, err := filepath.Abs(s.Path)
	if err != nil {
		return err
	}
	mutexValue, _ := processLocks.LoadOrStore(path, &sync.Mutex{})
	mutex := mutexValue.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()

	dir := filepath.Dir(s.Path)
	if err := ensureRegistryDir(dir); err != nil {
		return err
	}

	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := lock.Chmod(0o600); err != nil {
		return err
	}
	if err := flockExclusive(lock); err != nil {
		return err
	}
	defer flockRelease(lock)

	return fn()
}

func ensureRegistryDir(dir string) error {
	_, err := os.Stat(dir)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}
