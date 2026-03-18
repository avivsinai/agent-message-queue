// internal/discover/cache_test.go
package discover

import (
	"path/filepath"
	"testing"
)

func TestCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c := Cache{Entries: []CacheEntry{{Slug: "my-app", Dir: "/tmp/my-app"}}}
	if err := SaveCache(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Slug != "my-app" {
		t.Fatalf("unexpected: %+v", loaded)
	}
}

func TestCache_FindBySlug(t *testing.T) {
	c := Cache{Entries: []CacheEntry{
		{Slug: "app-a", Dir: "/a"},
		{Slug: "app-b", Dir: "/b"},
	}}
	entry, ok := c.FindBySlug("app-b")
	if !ok || entry.Dir != "/b" {
		t.Fatalf("unexpected: %+v", entry)
	}
	_, ok = c.FindBySlug("nope")
	if ok {
		t.Fatal("should not find nonexistent slug")
	}
}

func TestCache_MissingFile(t *testing.T) {
	c, err := LoadCache("/nonexistent/cache.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Entries) != 0 {
		t.Fatal("should be empty cache")
	}
}
