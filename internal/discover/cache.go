// internal/discover/cache.go
package discover

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const cacheVersion = 1

// CacheEntry represents a cached project discovery result.
type CacheEntry struct {
	Slug       string    `json:"slug"`
	ProjectID  string    `json:"project_id,omitempty"`
	Dir        string    `json:"dir"`
	AmqrcPath  string    `json:"amqrc_path"`
	BaseRoot   string    `json:"base_root"`
	AmqrcHash  string    `json:"amqrc_hash"`
	VerifiedAt time.Time `json:"verified_at"`
}

// Cache holds discovery cache state.
type Cache struct {
	Version int          `json:"version"`
	Entries []CacheEntry `json:"entries"`
}

// DefaultCachePath returns the default cache file path.
func DefaultCachePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "amq", "discovery-v1.json")
}

// LoadCache reads the discovery cache from disk.
func LoadCache(path string) (Cache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Cache{Version: cacheVersion}, nil // empty cache on missing file
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return Cache{Version: cacheVersion}, nil // reset on corrupt cache
	}
	if c.Version != cacheVersion {
		return Cache{Version: cacheVersion}, nil
	}
	return c, nil
}

// SaveCache writes the discovery cache to disk.
func SaveCache(path string, c Cache) error {
	c.Version = cacheVersion
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// Validate checks if a cache entry is still valid by verifying the .amqrc file.
func (e CacheEntry) Validate() bool {
	data, err := os.ReadFile(e.AmqrcPath)
	if err != nil {
		return false
	}
	return hashBytes(data) == e.AmqrcHash
}

// FindBySlug returns the first cache entry matching the given slug.
func (c Cache) FindBySlug(slug string) (CacheEntry, bool) {
	for _, e := range c.Entries {
		if e.Slug == slug {
			return e, true
		}
	}
	return CacheEntry{}, false
}

// Update adds or refreshes a cache entry for a discovered project.
func (c *Cache) Update(proj Project) {
	data, _ := os.ReadFile(proj.AmqrcPath)
	entry := CacheEntry{
		Slug:       proj.Slug,
		ProjectID:  proj.ProjectID,
		Dir:        proj.Dir,
		AmqrcPath:  proj.AmqrcPath,
		BaseRoot:   proj.BaseRoot,
		AmqrcHash:  hashBytes(data),
		VerifiedAt: time.Now(),
	}

	for i, e := range c.Entries {
		if e.Dir == proj.Dir {
			c.Entries[i] = entry
			return
		}
	}
	c.Entries = append(c.Entries, entry)
}

// Prune removes entries that no longer validate.
func (c *Cache) Prune() {
	valid := make([]CacheEntry, 0, len(c.Entries))
	for _, e := range c.Entries {
		if e.Validate() {
			valid = append(valid, e)
		}
	}
	c.Entries = valid
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8])
}
