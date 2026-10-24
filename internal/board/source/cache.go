package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// CacheKey is the exact identity of a forge cache entry. An entry serves a
// read only when every field matches, so a read under another account, repo
// set, or query schema is a miss.
type CacheKey struct {
	Host    string   `json:"host"`
	Repos   []string `json:"repos"`
	Account string   `json:"account"`
	Schema  string   `json:"schema"`
}

func (k CacheKey) equal(o CacheKey) bool {
	return k.Host == o.Host && k.Account == o.Account && k.Schema == o.Schema && slices.Equal(k.Repos, o.Repos)
}

// Cache stores forge responses. It never stores a token.
type Cache interface {
	Get(key CacheKey, now time.Time, ttl time.Duration) ([]byte, time.Time, bool)
	Put(key CacheKey, fetchedAt time.Time, response []byte) error
}

// DirCache keeps one file per key in Dir, named by the key's hash, holding
// the key itself so a read checks it.
type DirCache struct {
	Dir string
}

type cacheEntry struct {
	Key       CacheKey        `json:"key"`
	FetchedAt time.Time       `json:"fetched_at"`
	Response  json.RawMessage `json:"response"`
}

func (c DirCache) path(key CacheKey) string {
	data, _ := json.Marshal(key)
	sum := sha256.Sum256(data)
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:])+".json")
}

// Get returns the response stored for exactly key when it is younger than
// ttl.
func (c DirCache) Get(key CacheKey, now time.Time, ttl time.Duration) ([]byte, time.Time, bool) {
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, time.Time{}, false
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil || !e.Key.equal(key) {
		return nil, time.Time{}, false
	}
	if age := now.Sub(e.FetchedAt); age < 0 || age >= ttl {
		return nil, time.Time{}, false
	}
	return e.Response, e.FetchedAt, true
}

// Put writes the entry atomically (temp file plus rename), mode 0600 in a
// 0700 directory that must not be a symlink.
func (c DirCache) Put(key CacheKey, fetchedAt time.Time, response []byte) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(c.Dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a plain directory", c.Dir)
	}
	data, err := json.Marshal(cacheEntry{Key: key, FetchedAt: fetchedAt.UTC(), Response: response})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.Dir, ".entry-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, c.path(key)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
