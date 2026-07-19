package registry

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreUpsertRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".amq-keepalive", "registry.json")
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	store := New(path)
	store.Now = func() time.Time { return now }

	entry, err := store.Upsert(Entry{
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "file",
		Target:  "/tmp/inbox.txt",
	})
	if err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if entry.ID == "" {
		t.Fatal("entry ID is empty")
	}
	if entry.State != StateAttached {
		t.Fatalf("state = %q, want %q", entry.State, StateAttached)
	}
	if !entry.LastAttach.Equal(now) {
		t.Fatalf("LastAttach = %v, want %v", entry.LastAttach, now)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.SchemaVersion != SchemaVersion {
		t.Fatalf("schema = %d, want %d", loaded.SchemaVersion, SchemaVersion)
	}
	if len(loaded.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(loaded.Entries))
	}
	if loaded.Entries[0].ID != entry.ID {
		t.Fatalf("loaded ID = %q, want %q", loaded.Entries[0].ID, entry.ID)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat registry: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %v, want 0600", got)
	}
}

func TestStoreForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store := New(path)
	entry, err := store.Upsert(Entry{
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "file",
		Target:  "/tmp/inbox.txt",
	})
	if err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	removed, err := store.Forget(entry.ID)
	if err != nil {
		t.Fatalf("Forget() error = %v", err)
	}
	if !removed {
		t.Fatal("Forget() removed = false, want true")
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(loaded.Entries))
	}
}

func TestStoreForgetManyRemovesRequestedEntriesInOneSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store := New(path)
	var ids []string
	for _, agent := range []string{"codex", "claude", "observer"} {
		entry, err := store.Upsert(Entry{Root: "/tmp/amq-root", Agent: agent, Adapter: "file", Target: "/tmp/" + agent})
		if err != nil {
			t.Fatalf("Upsert(%s): %v", agent, err)
		}
		ids = append(ids, entry.ID)
	}
	removed, err := store.ForgetMany(ids[:2])
	if err != nil || removed != 2 {
		t.Fatalf("ForgetMany removed=%d err=%v", removed, err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Agent != "observer" {
		t.Fatalf("entries=%#v, want observer only", loaded.Entries)
	}
}

func TestStoreForgetManyRefusesPartialMatchWithoutRemovingAnything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store := New(path)
	entry, err := store.Upsert(Entry{Root: "/tmp/amq-root", Agent: "codex", Adapter: "file", Target: "/tmp/codex"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	removed, err := store.ForgetMany([]string{entry.ID, "missing-id"})
	if err == nil || removed != 0 {
		t.Fatalf("ForgetMany removed=%d err=%v, want refusal", removed, err)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0].ID != entry.ID {
		t.Fatalf("registry changed after partial-match refusal: entries=%#v err=%v", loaded.Entries, loadErr)
	}
}

func TestStoreDoesNotChmodExistingCustomRegistryDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custom")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	store := New(filepath.Join(dir, "registry.json"))
	_, err := store.Upsert(Entry{
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "file",
		Target:  "/tmp/inbox.txt",
	})
	if err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("dir mode = %v, want existing 0755 preserved", got)
	}
}

func TestStoreReplaceSessionAdapterRemovesAllEntriesForRootAndAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store := New(path)

	replaceMe, err := store.Upsert(Entry{
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "file",
		Target:  "/tmp/old-inbox.txt",
	})
	if err != nil {
		t.Fatalf("Upsert(replaceMe) error = %v", err)
	}
	keepDifferentAgent, err := store.Upsert(Entry{
		Root:    "/tmp/amq-root",
		Agent:   "claude",
		Adapter: "file",
		Target:  "/tmp/claude-inbox.txt",
	})
	if err != nil {
		t.Fatalf("Upsert(keepDifferentAgent) error = %v", err)
	}
	replaceDifferentAdapter, err := store.Upsert(Entry{
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "ghostty",
		Target:  "ghostty:terminal:old",
	})
	if err != nil {
		t.Fatalf("Upsert(replaceDifferentAdapter) error = %v", err)
	}

	next, removed, err := store.ReplaceSessionAdapter(Entry{
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "cmux",
		Target:  "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3",
	})
	if err != nil {
		t.Fatalf("ReplaceSessionAdapter() error = %v", err)
	}
	if next.Adapter != "cmux" {
		t.Fatalf("Adapter = %q, want cmux", next.Adapter)
	}
	removedIDs := map[string]bool{}
	for _, entry := range removed {
		removedIDs[entry.ID] = true
	}
	if len(removed) != 2 || !removedIDs[replaceMe.ID] || !removedIDs[replaceDifferentAdapter.ID] {
		t.Fatalf("removed = %#v, want old file and Ghostty entries", removed)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(loaded.Entries))
	}
	ids := map[string]bool{}
	for _, entry := range loaded.Entries {
		ids[entry.ID] = true
		if entry.ID == replaceMe.ID || entry.ID == replaceDifferentAdapter.ID {
			t.Fatalf("old matching entry still present: %#v", entry)
		}
	}
	for _, want := range []string{keepDifferentAgent.ID, next.ID} {
		if !ids[want] {
			t.Fatalf("entry %q missing after replace; entries=%#v", want, loaded.Entries)
		}
	}
}

func TestStoreConcurrentUpsertsDoNotLoseEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store := New(path)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Upsert(Entry{
				Root:    "/tmp/amq-root",
				Agent:   "codex",
				Adapter: "file",
				Target:  filepath.Join("/tmp", "inbox", string(rune('a'+i))),
			})
			if err != nil {
				t.Errorf("Upsert(%d) error = %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Entries) != 20 {
		t.Fatalf("entries = %d, want 20", len(loaded.Entries))
	}
}

func TestStoreConcurrentSameTargetReplacementsConverge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	store := New(path)
	if _, err := store.Upsert(Entry{
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "ghostty",
		Target:  "ghostty:terminal:old",
	}); err != nil {
		t.Fatalf("Upsert(old) error = %v", err)
	}

	const target = "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := store.ReplaceSessionAdapter(Entry{
				Root:    "/tmp/amq-root",
				Agent:   "codex",
				Adapter: "cmux",
				Target:  target,
			}); err != nil {
				t.Errorf("ReplaceSessionAdapter() error = %v", err)
			}
		}()
	}
	wg.Wait()

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Adapter != "cmux" || loaded.Entries[0].Target != target {
		t.Fatalf("entries = %#v, want one converged cmux registration", loaded.Entries)
	}
}

func TestStoreCorruptRegistryReturnsTypedError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	store := New(path)

	_, err := store.Load()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load() error = %v, want ErrCorrupt", err)
	}
}
