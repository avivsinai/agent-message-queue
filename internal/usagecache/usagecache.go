// Package usagecache is the durable state of usage collection under
// ~/.amq/usage: the collector lock, the coordination journal, and the
// sanitized fact snapshot. Files are mode 0600 in a 0700 directory, every
// write is atomic and synced with its directory before it returns, and only
// facts and attempts are stored: no response body, transcript or credential.
package usagecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/usage"
	"github.com/avivsinai/agent-message-queue/usage/edge"
)

const (
	lockFile    = "collector.lock"
	journalFile = "coordination-v1.json"
	factsFile   = "facts-v1.json"
	// schema is the version both files carry; another version is refused.
	schema = 1
	// maxFile bounds a read: both files hold a few records per home.
	maxFile = 4 << 20
)

// Store is the state directory. It implements edge.Persistence.
type Store struct{ dir string }

var _ edge.Persistence = Store{}

// Open is the store at ~/.amq/usage.
func Open() (Store, error) {
	home, err := amqhome.EnsureDir()
	if err != nil {
		return Store{}, err
	}
	return New(filepath.Join(home, "usage"))
}

// New is the store at dir. A missing dir is created with mode 0700 and its
// parent synced; a symlink or a non-directory is refused.
func New(dir string) (Store, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return Store{}, err
		}
		if err := fsq.SyncDir(filepath.Dir(dir)); err != nil {
			return Store{}, err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return Store{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Store{}, fmt.Errorf("%s is not a plain directory; refusing", dir)
	}
	return Store{dir: dir}, nil
}

// TryLock takes the collector lock without waiting. Where advisory locks
// are unavailable it fails closed with an error, so no collection runs
// unpaced.
func (s Store) TryLock() (func(), bool, error) {
	if !lock.AdvisoryLockAvailable() {
		return nil, false, errors.New("advisory file locks are unavailable on this platform")
	}
	return lock.TryExclusive(filepath.Join(s.dir, lockFile))
}

type journalFileV1 struct {
	Schema int `json:"schema"`
	edge.Journal
}

type factsFileV1 struct {
	Schema int          `json:"schema"`
	Facts  usage.Result `json:"facts"`
}

// LoadJournal reads the journal. A missing file is an empty journal; an
// unreadable or corrupt one is an error.
func (s Store) LoadJournal() (edge.Journal, error) {
	var f journalFileV1
	ok, err := s.load(journalFile, &f)
	if err != nil || !ok {
		return edge.Journal{}, err
	}
	if f.Schema != schema {
		return edge.Journal{}, fmt.Errorf("%s: schema %d, want %d", journalFile, f.Schema, schema)
	}
	return f.Journal, nil
}

// SaveJournal replaces the journal atomically and durably.
func (s Store) SaveJournal(j edge.Journal) error {
	return s.save(journalFile, journalFileV1{Schema: schema, Journal: j})
}

// LoadFacts reads the fact snapshot. A missing file is an empty Result.
func (s Store) LoadFacts() (usage.Result, error) {
	var f factsFileV1
	ok, err := s.load(factsFile, &f)
	if err != nil || !ok {
		return usage.Result{}, err
	}
	if f.Schema != schema {
		return usage.Result{}, fmt.Errorf("%s: schema %d, want %d", factsFile, f.Schema, schema)
	}
	return f.Facts, nil
}

// SaveFacts replaces the fact snapshot atomically and durably.
func (s Store) SaveFacts(r usage.Result) error {
	return s.save(factsFile, factsFileV1{Schema: schema, Facts: r})
}

func (s Store) load(name string, v any) (bool, error) {
	f, err := os.Open(filepath.Join(s.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return false, err
	}
	if len(data) > maxFile {
		return false, fmt.Errorf("%s: over %d bytes", name, maxFile)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return true, nil
}

func (s Store) save(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(s.dir, name, append(data, '\n'), 0o600)
	return err
}
