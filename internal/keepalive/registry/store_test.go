package registry

import (
	"context"
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

func TestStoreRejectsSecondOwnerForSameAdapterTarget(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "registry.json"))
	target := "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"
	if _, err := store.Upsert(Entry{Root: "/tmp/first", Agent: "codex", Adapter: "cmux", Target: target}); err != nil {
		t.Fatalf("Upsert(first) error = %v", err)
	}
	_, err := store.Upsert(Entry{Root: "/tmp/second", Agent: "claude", Adapter: "cmux", Target: target})
	if !errors.Is(err, ErrTargetOwned) {
		t.Fatalf("Upsert(second) error = %v, want ErrTargetOwned", err)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0].Root != "/tmp/first" {
		t.Fatalf("registry changed after collision: entries=%#v err=%v", loaded.Entries, loadErr)
	}
}

func TestStoreRejectsCanonicalCmuxTargetOwnedByLegacyLowercaseRow(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "registry.json"))
	lower := "cmux:surface:f901d722-6789-4bbb-9818-c4e97f20beb3"
	legacy := Entry{
		ID: EntryID("/tmp/first", "codex", "cmux", lower), Root: "/tmp/first", Agent: "codex",
		Adapter: "cmux", Target: lower, State: StateActive,
	}
	if err := store.Save(File{SchemaVersion: SchemaVersion, Entries: []Entry{legacy}}); err != nil {
		t.Fatalf("Save legacy row: %v", err)
	}
	upper := "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"
	_, err := store.Upsert(Entry{Root: "/tmp/second", Agent: "claude", Adapter: "cmux", Target: upper})
	if !errors.Is(err, ErrTargetOwned) {
		t.Fatalf("Upsert(canonical) error = %v, want ErrTargetOwned", err)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != legacy {
		t.Fatalf("legacy registry changed: entries=%#v err=%v", loaded.Entries, loadErr)
	}
}

func TestRegistrationLockWaitHonorsContextCancellation(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "registry.json"))
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithRegistrationLock(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	called := false
	err := store.WithRegistrationLockContext(ctx, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("wait error=%v called=%v, want canceled acquisition", err, called)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("holder error: %v", err)
	}
}

func TestStoreReplacePreflightRejectsTargetOwnedByDifferentSession(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "registry.json"))
	target := "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"
	if _, err := store.Upsert(Entry{Root: "/tmp/first", Agent: "codex", Adapter: "cmux", Target: target}); err != nil {
		t.Fatalf("Upsert(first) error = %v", err)
	}
	err := store.CheckTargetAvailable(Entry{Root: "/tmp/second", Agent: "codex", Adapter: "cmux", Target: target}, true)
	if !errors.Is(err, ErrTargetOwned) {
		t.Fatalf("CheckTargetAvailable() error = %v, want ErrTargetOwned", err)
	}
}

func TestStoreBatchUpdateCASPreservesConcurrentChangesAndNewEntries(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "registry.json"))
	first, err := store.Upsert(Entry{Root: "/tmp/first", Agent: "codex", Adapter: "file", Target: "/tmp/first.txt"})
	if err != nil {
		t.Fatalf("Upsert(first): %v", err)
	}
	second, err := store.Upsert(Entry{Root: "/tmp/second", Agent: "codex", Adapter: "file", Target: "/tmp/second.txt"})
	if err != nil {
		t.Fatalf("Upsert(second): %v", err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatalf("Load snapshot: %v", err)
	}
	firstBefore, _ := findTestEntry(snapshot.Entries, first.ID)
	secondBefore, _ := findTestEntry(snapshot.Entries, second.ID)

	firstConcurrent := firstBefore
	firstConcurrent.LastError = "concurrent reattach won"
	if err := store.UpdateEntry(firstConcurrent); err != nil {
		t.Fatalf("UpdateEntry(concurrent): %v", err)
	}
	third, err := store.Upsert(Entry{Root: "/tmp/third", Agent: "codex", Adapter: "file", Target: "/tmp/third.txt"})
	if err != nil {
		t.Fatalf("Upsert(third): %v", err)
	}
	firstAfter := firstBefore
	firstAfter.State = StateActive
	secondAfter := secondBefore
	secondAfter.State = StateActive
	result, err := store.UpdateEntries([]EntryUpdate{
		{Before: firstBefore, After: firstAfter},
		{Before: secondBefore, After: secondAfter},
	})
	if err != nil {
		t.Fatalf("UpdateEntries: %v", err)
	}
	if result.Updated != 1 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want one update and one stale skip", result)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load final: %v", err)
	}
	gotFirst, ok := findTestEntry(loaded.Entries, first.ID)
	if !ok || gotFirst.LastError != firstConcurrent.LastError || gotFirst.State == StateActive {
		t.Fatalf("concurrent first entry was clobbered: %#v", gotFirst)
	}
	gotSecond, ok := findTestEntry(loaded.Entries, second.ID)
	if !ok || gotSecond.State != StateActive {
		t.Fatalf("second entry was not updated: %#v", gotSecond)
	}
	if _, ok := findTestEntry(loaded.Entries, third.ID); !ok {
		t.Fatalf("concurrently added third entry was lost: %#v", loaded.Entries)
	}
}

func TestStoreForgetIfUnchangedSkipsNewerState(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "registry.json"))
	entry, err := store.Upsert(Entry{Root: "/tmp/root", Agent: "codex", Adapter: "file", Target: "/tmp/inbox.txt"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	newer := entry
	newer.LastError = "newer state"
	if err := store.UpdateEntry(newer); err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}
	removed, err := store.ForgetIfUnchanged(entry)
	if err != nil || removed {
		t.Fatalf("ForgetIfUnchanged removed=%v err=%v, want safe skip", removed, err)
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

func TestStoreRestoresPreviousRowsOnlyWhileReservationIsUnchanged(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "registry.json"))
	previous, err := store.Upsert(Entry{Root: "/tmp/root", Agent: "codex", Adapter: "file", Target: "/tmp/old"})
	if err != nil {
		t.Fatalf("Upsert previous: %v", err)
	}
	reservation, removed, err := store.ReplaceSessionAdapter(Entry{
		Root: "/tmp/root", Agent: "codex", Adapter: "file", Target: "/tmp/new", State: StateAttached,
	})
	if err != nil || len(removed) != 1 || removed[0] != previous {
		t.Fatalf("Replace reservation=%#v removed=%#v err=%v", reservation, removed, err)
	}
	restored, err := store.RestoreSessionAdapterIfUnchanged(reservation, removed)
	if err != nil || !restored {
		t.Fatalf("RestoreSessionAdapterIfUnchanged restored=%v err=%v", restored, err)
	}
	loaded, err := store.Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != previous {
		t.Fatalf("restored entries=%#v err=%v", loaded.Entries, err)
	}

	reservation, removed, err = store.ReplaceSessionAdapter(Entry{
		Root: "/tmp/root", Agent: "codex", Adapter: "file", Target: "/tmp/new", State: StateAttached,
	})
	if err != nil {
		t.Fatalf("Replace second reservation: %v", err)
	}
	changed := reservation
	changed.LastError = "supervisor observed reservation"
	if err := store.UpdateEntry(changed); err != nil {
		t.Fatalf("Update reservation: %v", err)
	}
	restored, err = store.RestoreSessionAdapterIfUnchanged(reservation, removed)
	if err != nil || restored {
		t.Fatalf("changed reservation restored=%v err=%v, want safe skip", restored, err)
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

func findTestEntry(entries []Entry, id string) (Entry, bool) {
	for _, entry := range entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return Entry{}, false
}
