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
	path := registryTestPath(t)
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
	path := registryTestPath(t)
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
	path := registryTestPath(t)
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
	store := New(registryTestPath(t))
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

func TestStoreRejectsSecondRegistrationForSameRootAndAgentAcrossAdapters(t *testing.T) {
	store := New(registryTestPath(t))
	first, err := store.Upsert(Entry{Root: "/tmp/amq-root", Agent: "codex", Adapter: "file", Target: "/tmp/first.txt"})
	if err != nil {
		t.Fatalf("Upsert(first) error = %v", err)
	}
	_, err = store.Upsert(Entry{Root: "/tmp/amq-root", Agent: "codex", Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"})
	if !errors.Is(err, ErrRegistrationOwned) {
		t.Fatalf("Upsert(second) error = %v, want ErrRegistrationOwned", err)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != first {
		t.Fatalf("registry changed after second-registration refusal: entries=%#v err=%v", loaded.Entries, loadErr)
	}
}

func TestStoreUsesCanonicalRootForRegistrationOwnershipAndReattach(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	realRoot := filepath.Join(dir, "real-root")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatalf("Mkdir real root: %v", err)
	}
	aliasRoot := filepath.Join(dir, "alias-root")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatalf("Symlink root alias: %v", err)
	}
	canonicalRoot, err := CanonicalRoot(realRoot)
	if err != nil {
		t.Fatalf("CanonicalRoot(real root): %v", err)
	}
	store := New(filepath.Join(dir, "registry.json"))
	first, err := store.Upsert(Entry{Root: aliasRoot, Agent: "codex", Adapter: "file", Target: filepath.Join(dir, "first.txt")})
	if err != nil {
		t.Fatalf("Upsert(first) error = %v", err)
	}
	if first.Root != canonicalRoot {
		t.Fatalf("first root = %q, want canonical %q", first.Root, canonicalRoot)
	}
	_, err = store.Upsert(Entry{Root: realRoot, Agent: "codex", Adapter: "ghostty", Target: "ghostty:terminal:second"})
	if !errors.Is(err, ErrRegistrationOwned) {
		t.Fatalf("Upsert(alias collision) error = %v, want ErrRegistrationOwned", err)
	}

	replacement, removed, err := store.ReplaceSessionAdapter(Entry{Root: realRoot, Agent: "codex", Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"})
	if err != nil {
		t.Fatalf("ReplaceSessionAdapter() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != first {
		t.Fatalf("removed=%#v, want canonical prior registration %#v", removed, first)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != replacement || replacement.Root != canonicalRoot {
		t.Fatalf("reattach did not converge canonical alias: entries=%#v replacement=%#v err=%v", loaded.Entries, replacement, loadErr)
	}
}

func TestStoreUpsertRewritesEquivalentLegacyRootAlias(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	realRoot := filepath.Join(dir, "real-root")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatalf("Mkdir real root: %v", err)
	}
	aliasRoot := filepath.Join(dir, "alias-root")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatalf("Symlink root alias: %v", err)
	}
	canonicalRoot, err := CanonicalRoot(realRoot)
	if err != nil {
		t.Fatalf("CanonicalRoot(real root): %v", err)
	}
	target := filepath.Join(dir, "inbox.txt")
	legacy := Entry{
		ID:      EntryID(aliasRoot, "codex", "file", target),
		Root:    aliasRoot,
		Agent:   "codex",
		Adapter: "file",
		Target:  target,
		State:   StateActive,
	}
	store := New(filepath.Join(dir, "registry.json"))
	if err := store.Save(File{Entries: []Entry{legacy}}); err != nil {
		t.Fatalf("Save legacy entry: %v", err)
	}
	rewritten, err := store.Upsert(Entry{Root: realRoot, Agent: "codex", Adapter: "file", Target: target})
	if err != nil {
		t.Fatalf("Upsert equivalent registration: %v", err)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != rewritten || rewritten.Root != canonicalRoot {
		t.Fatalf("legacy alias did not converge: entries=%#v rewritten=%#v err=%v", loaded.Entries, rewritten, loadErr)
	}
}

func TestStoreRejectsCanonicalCmuxTargetOwnedByLegacyLowercaseRow(t *testing.T) {
	store := New(registryTestPath(t))
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

func TestStoreRejectsCanonicalGhosttyTargetOwnedByLegacyLowercaseRow(t *testing.T) {
	store := New(registryTestPath(t))
	legacy := Entry{
		ID: EntryID("/tmp/first", "codex", "ghostty", "ghostty:terminal:terminal-1"), Root: "/tmp/first", Agent: "codex",
		Adapter: "ghostty", Target: "ghostty:terminal:terminal-1", State: StateActive,
	}
	if err := store.Save(File{Entries: []Entry{legacy}}); err != nil {
		t.Fatalf("Save legacy row: %v", err)
	}
	_, err := store.Upsert(Entry{Root: "/tmp/second", Agent: "claude", Adapter: "ghostty", Target: "ghostty:terminal:TERMINAL-1"})
	if !errors.Is(err, ErrTargetOwned) {
		t.Fatalf("Upsert(canonical Ghostty) error = %v, want ErrTargetOwned", err)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != legacy {
		t.Fatalf("legacy Ghostty row changed: entries=%#v err=%v", loaded.Entries, loadErr)
	}
}

func TestStoreRejectsFileTargetOwnedByLegacySymlinkAlias(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(dir, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("Mkdir real parent: %v", err)
	}
	aliasParent := filepath.Join(dir, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatalf("Symlink file target parent: %v", err)
	}
	legacy := Entry{
		ID: EntryID("/tmp/first", "codex", "file", filepath.Join(aliasParent, "inbox.txt")), Root: "/tmp/first", Agent: "codex",
		Adapter: "file", Target: filepath.Join(aliasParent, "inbox.txt"), State: StateActive,
	}
	store := New(filepath.Join(dir, "registry.json"))
	if err := store.Save(File{Entries: []Entry{legacy}}); err != nil {
		t.Fatalf("Save legacy row: %v", err)
	}
	_, err := store.Upsert(Entry{
		Root: "/tmp/second", Agent: "claude", Adapter: "file", Target: filepath.Join(realParent, "inbox.txt"),
	})
	if !errors.Is(err, ErrTargetOwned) {
		t.Fatalf("Upsert(canonical file target) error = %v, want ErrTargetOwned", err)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != legacy {
		t.Fatalf("legacy file row changed: entries=%#v err=%v", loaded.Entries, loadErr)
	}
}

func TestRegistrationLockWaitHonorsContextCancellation(t *testing.T) {
	store := New(registryTestPath(t))
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
	store := New(registryTestPath(t))
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
	store := New(registryTestPath(t))
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
	store := New(registryTestPath(t))
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

func TestStoreReplaceSessionAdapterRemovesAllEntriesForRootAndAgent(t *testing.T) {
	path := registryTestPath(t)
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
	replaceDifferentAdapter := Entry{
		ID:      EntryID("/tmp/amq-root", "codex", "ghostty", "ghostty:terminal:old"),
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "ghostty",
		Target:  "ghostty:terminal:old",
		State:   StateAttached,
	}
	legacy, err := store.Load()
	if err != nil {
		t.Fatalf("Load legacy registry: %v", err)
	}
	legacy.Entries = append(legacy.Entries, replaceDifferentAdapter)
	if err := store.Save(legacy); err != nil {
		t.Fatalf("Save legacy duplicate registration: %v", err)
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
	store := New(registryTestPath(t))
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
	path := registryTestPath(t)
	store := New(path)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Upsert(Entry{
				Root:    "/tmp/amq-root",
				Agent:   string(rune('a' + i)),
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
	path := registryTestPath(t)
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
	path := registryTestPath(t)
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	store := New(path)

	_, err := store.Load()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load() error = %v, want ErrCorrupt", err)
	}
}

func TestStoreLoadsSchemaZeroAsCurrent(t *testing.T) {
	path := registryTestPath(t)
	if err := os.WriteFile(path, []byte(`{"entries":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := New(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.SchemaVersion != SchemaVersion {
		t.Fatalf("schema = %d, want %d", loaded.SchemaVersion, SchemaVersion)
	}
}

func TestStoreSaveReportsRenameFailure(t *testing.T) {
	path := registryTestPath(t)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir registry destination: %v", err)
	}
	if err := New(path).Save(File{}); err == nil {
		t.Fatal("Save error = nil, want rename failure for directory destination")
	}
}

func TestStoreSaveReportsCommittedDurabilityError(t *testing.T) {
	previous := finishCommittedRegistry
	t.Cleanup(func() { finishCommittedRegistry = previous })
	finishCommittedRegistry = func(path, _ string) error {
		return &CommittedDurabilityError{Path: path, Err: errors.New("sync failed")}
	}
	path := registryTestPath(t)
	err := New(path).Save(File{})
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) || committed.Path != path {
		t.Fatalf("Save error = %T %v, want CommittedDurabilityError for %q", err, err, path)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("committed registry missing after durability error: %v", statErr)
	}
}

func TestStoreSaveWrapsPostRenameChmodAndSyncFailures(t *testing.T) {
	previousChmod, previousSync := chmodRegistryFile, syncRegistryDir
	t.Cleanup(func() {
		chmodRegistryFile = previousChmod
		syncRegistryDir = previousSync
	})

	for _, test := range []struct {
		name  string
		setup func()
	}{
		{
			name: "chmod",
			setup: func() {
				chmodRegistryFile = func(string, os.FileMode) error { return errors.New("chmod failed") }
				syncRegistryDir = previousSync
			},
		},
		{
			name: "sync",
			setup: func() {
				chmodRegistryFile = previousChmod
				syncRegistryDir = func(string) error { return errors.New("sync failed") }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.setup()
			path := registryTestPath(t)
			err := New(path).Save(File{})
			var committed *CommittedDurabilityError
			if !errors.As(err, &committed) || committed.Path != path {
				t.Fatalf("Save error = %T %v, want committed durability error for %q", err, err, path)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("committed registry missing after %s failure: %v", test.name, statErr)
			}
		})
	}
}

func TestStoreRestoreMissingReservationIsNoOp(t *testing.T) {
	store := New(registryTestPath(t))
	existing, err := store.Upsert(Entry{Root: "/tmp/root", Agent: "codex", Adapter: "file", Target: "/tmp/current"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	missing := existing
	missing.ID = "missing-reservation"
	restored, err := store.RestoreSessionAdapterIfUnchanged(missing, nil)
	if err != nil || restored {
		t.Fatalf("RestoreSessionAdapterIfUnchanged restored=%v err=%v, want no-op", restored, err)
	}
	loaded, err := store.Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != existing {
		t.Fatalf("registry changed: entries=%#v err=%v", loaded.Entries, err)
	}
}

func TestStoreUpdateEntriesRejectsEitherMissingID(t *testing.T) {
	store := New(registryTestPath(t))
	before := Entry{ID: "entry-1"}
	after := before
	for _, test := range []struct {
		name   string
		before Entry
		after  Entry
	}{
		{name: "before", before: Entry{}, after: after},
		{name: "after", before: before, after: Entry{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.UpdateEntries([]EntryUpdate{{Before: test.before, After: test.after}}); err == nil {
				t.Fatal("UpdateEntries error = nil, want missing-ID refusal")
			}
		})
	}
}

func TestStoreUpdatesRejectIdentityFieldChanges(t *testing.T) {
	store := New(registryTestPath(t))
	entry, err := store.Upsert(Entry{Root: "/tmp/root", Agent: "codex", Adapter: "file", Target: "/tmp/inbox"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	changed := entry
	changed.Agent = "claude"
	if err := store.UpdateEntry(changed); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("UpdateEntry identity change error = %v, want ErrInvalidIdentity", err)
	}

	changed = entry
	changed.Target = "/tmp/other"
	result, err := store.UpdateEntries([]EntryUpdate{{Before: entry, After: changed}})
	if !errors.Is(err, ErrInvalidIdentity) || result.Updated != 0 {
		t.Fatalf("UpdateEntries identity change result=%+v err=%v, want immutable identity refusal", result, err)
	}
	loaded, err := store.Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != entry {
		t.Fatalf("identity change mutated registry: entries=%#v err=%v", loaded.Entries, err)
	}
}

func TestStoreForgetIfUnchangedDoesNotRemoveAnotherEntry(t *testing.T) {
	store := New(registryTestPath(t))
	first, err := store.Upsert(Entry{Root: "/tmp/first", Agent: "codex", Adapter: "file", Target: "/tmp/first"})
	if err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	second, err := store.Upsert(Entry{Root: "/tmp/second", Agent: "claude", Adapter: "file", Target: "/tmp/second"})
	if err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	missing := second
	missing.ID = "missing-entry"
	removed, err := store.ForgetIfUnchanged(missing)
	if err != nil || removed {
		t.Fatalf("ForgetIfUnchanged removed=%v err=%v, want no-op", removed, err)
	}
	loaded, err := store.Load()
	_, hasFirst := findTestEntry(loaded.Entries, first.ID)
	_, hasSecond := findTestEntry(loaded.Entries, second.ID)
	if err != nil || len(loaded.Entries) != 2 || !hasFirst || !hasSecond {
		t.Fatalf("registry changed: entries=%#v err=%v", loaded.Entries, err)
	}
}

func TestStoreForgetIfUnchangedRemovesExactLaterEntry(t *testing.T) {
	store := New(registryTestPath(t))
	first, err := store.Upsert(Entry{Root: "/tmp/first", Agent: "codex", Adapter: "file", Target: "/tmp/first"})
	if err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	second, err := store.Upsert(Entry{Root: "/tmp/second", Agent: "claude", Adapter: "file", Target: "/tmp/second"})
	if err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	removed, err := store.ForgetIfUnchanged(second)
	if err != nil || !removed {
		t.Fatalf("ForgetIfUnchanged removed=%v err=%v, want exact removal", removed, err)
	}
	loaded, err := store.Load()
	if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != first {
		t.Fatalf("entries=%#v err=%v, want first only", loaded.Entries, err)
	}
}

func TestStoreSortsEntriesByID(t *testing.T) {
	store := New(registryTestPath(t))
	second, err := store.Upsert(Entry{Root: "/tmp/z", Agent: "codex", Adapter: "file", Target: "/tmp/z"})
	if err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	first, err := store.Upsert(Entry{Root: "/tmp/a", Agent: "claude", Adapter: "file", Target: "/tmp/a"})
	if err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 2 {
		t.Fatalf("entries = %#v, want 2", loaded.Entries)
	}
	if loaded.Entries[0].ID > loaded.Entries[1].ID {
		t.Fatalf("entries = %#v, want ascending IDs", loaded.Entries)
	}
	seen := map[string]bool{loaded.Entries[0].ID: true, loaded.Entries[1].ID: true}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatalf("entries = %#v, want IDs %q and %q", loaded.Entries, first.ID, second.ID)
	}
}

func TestStoreRegistrationOwnershipRequiresSameAdapterAndTarget(t *testing.T) {
	for _, candidate := range []Entry{
		{Root: "/tmp/root", Agent: "codex", Adapter: "file", Target: "/tmp/different"},
		{Root: "/tmp/root", Agent: "codex", Adapter: "ghostty", Target: "/tmp/original"},
	} {
		store := New(registryTestPath(t))
		original, err := store.Upsert(Entry{Root: "/tmp/root", Agent: "codex", Adapter: "file", Target: "/tmp/original"})
		if err != nil {
			t.Fatalf("Upsert original: %v", err)
		}
		if _, err := store.Upsert(candidate); !errors.Is(err, ErrRegistrationOwned) {
			t.Fatalf("Upsert(%+v) error = %v, want ErrRegistrationOwned", candidate, err)
		}
		loaded, err := store.Load()
		if err != nil || len(loaded.Entries) != 1 || loaded.Entries[0] != original {
			t.Fatalf("registry changed: entries=%#v err=%v", loaded.Entries, err)
		}
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

func registryTestPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".amq-keepalive")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir registry test dir: %v", err)
	}
	return filepath.Join(dir, "registry.json")
}
