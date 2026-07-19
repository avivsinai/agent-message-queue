package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	SchemaVersion = 1

	StateAttached State = "attached"
	StateActive   State = "active"
	StateDetached State = "detached"
	StateStale    State = "stale"
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

var processLocks sync.Map

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
	sum := sha256.Sum256([]byte(root + "\x00" + agent + "\x00" + adapterName + "\x00" + target))
	return hex.EncodeToString(sum[:])
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
		next := make([]Entry, 0, len(file.Entries)+1)
		for _, existing := range file.Entries {
			// AMQ permits one wake process per root and agent. Reattach therefore
			// replaces the old registration even when the terminal adapter changed.
			if existing.Root == prepared.Root && existing.Agent == prepared.Agent {
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

func (s *Store) prepareEntry(entry Entry) (Entry, error) {
	now := s.now()
	if entry.Root == "" {
		return Entry{}, errors.New("entry root is required")
	}
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

func (s *Store) Forget(id string) (bool, error) {
	removed, err := s.ForgetMany([]string{id})
	return removed == 1, err
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
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

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
