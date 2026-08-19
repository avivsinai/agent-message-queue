//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStoreRefusesHostileRegistryIdentity(t *testing.T) {
	sample := Entry{Root: "/tmp/amq-root", Agent: "codex", Adapter: "file", Target: "/tmp/inbox.txt"}

	t.Run("symlink dir", func(t *testing.T) {
		base := t.TempDir()
		realDir := filepath.Join(base, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		linkDir := filepath.Join(base, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		_, err := New(filepath.Join(linkDir, "registry.json")).Upsert(sample)
		if err == nil {
			t.Fatal("Upsert error = nil, want symlink registry dir refusal")
		}
		if _, statErr := os.Stat(filepath.Join(realDir, "registry.json")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("followed symlink registry dir: stat err=%v", statErr)
		}
	})

	t.Run("wrong mode", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "registry-dir")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		if _, err := New(filepath.Join(dir, "registry.json")).Upsert(sample); err == nil {
			t.Fatal("Upsert error = nil, want 0755 registry dir refusal")
		}
	})

	t.Run("wrong owner", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "registry-dir")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.Lchown(dir, os.Getuid()+1, -1); err != nil {
			t.Skipf("cannot chown registry dir: %v", err)
		}
		if _, err := New(filepath.Join(dir, "registry.json")).Upsert(sample); err == nil {
			t.Fatal("Upsert error = nil, want foreign-owner registry dir refusal")
		}
	})

	t.Run("lock symlink", func(t *testing.T) {
		dir := t.TempDir()
		victim := filepath.Join(dir, "victim")
		if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Symlink(victim, filepath.Join(dir, "registry.json.lock")); err != nil {
			t.Fatalf("Symlink lock: %v", err)
		}
		if _, err := New(filepath.Join(dir, "registry.json")).Upsert(sample); err == nil {
			t.Fatal("Upsert error = nil, want nofollow lock refusal")
		}
		got, err := os.ReadFile(victim)
		if err != nil || string(got) != "keep\n" {
			t.Fatalf("lock symlink followed: contents=%q err=%v", got, err)
		}
	})

	t.Run("lock replacement", func(t *testing.T) {
		dir := t.TempDir()
		lockPath := filepath.Join(dir, "registry.json.lock")
		lock, err := openPinnedLock(lockPath)
		if err != nil {
			t.Fatalf("openPinnedLock: %v", err)
		}
		defer func() { _ = lock.Close() }()
		if err := flockExclusive(lock); err != nil {
			t.Fatalf("flockExclusive: %v", err)
		}
		defer flockRelease(lock)
		if err := os.Remove(lockPath); err != nil {
			t.Fatalf("Remove lock: %v", err)
		}
		if err := os.WriteFile(lockPath, []byte("replacement\n"), 0o600); err != nil {
			t.Fatalf("WriteFile replacement: %v", err)
		}
		if err := recheckLockIdentity(lock, lockPath); err == nil {
			t.Fatal("recheckLockIdentity error = nil, want replacement refusal")
		}
	})

	t.Run("duplicate and mismatched ids", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "registry.json")
		valid := Entry{
			ID: EntryID("/tmp/first", "codex", "file", "/tmp/first"), Root: "/tmp/first",
			Agent: "codex", Adapter: "file", Target: "/tmp/first", State: StateAttached,
		}
		writeRawRegistry(t, path, []Entry{valid, valid})
		if _, err := New(path).Load(); !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("Load(duplicate id) error = %v, want identity refusal", err)
		}

		mismatched := valid
		mismatched.ID = "not-the-identity-hash"
		writeRawRegistry(t, path, []Entry{mismatched})
		if _, err := New(path).Load(); !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("Load(mismatched id) error = %v, want identity refusal", err)
		}

		store := New(filepath.Join(dir, "live.json"))
		victim, err := store.Upsert(valid)
		if err != nil {
			t.Fatalf("Upsert victim: %v", err)
		}
		_, err = store.Upsert(Entry{
			ID: victim.ID, Root: "/tmp/other", Agent: "claude", Adapter: "file", Target: "/tmp/other",
		})
		if !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("Upsert(stolen id) error = %v, want ErrInvalidIdentity", err)
		}
		loaded, loadErr := store.Load()
		if loadErr != nil || len(loaded.Entries) != 1 || loaded.Entries[0].ID != victim.ID {
			t.Fatalf("stolen-id upsert replaced victim: entries=%#v err=%v", loaded.Entries, loadErr)
		}
	})

	t.Run("ELOOP and EACCES", func(t *testing.T) {
		dir := t.TempDir()
		loopA := filepath.Join(dir, "loop-a")
		loopB := filepath.Join(dir, "loop-b")
		if err := os.Symlink(loopB, loopA); err != nil {
			t.Fatalf("Symlink A: %v", err)
		}
		if err := os.Symlink(loopA, loopB); err != nil {
			t.Fatalf("Symlink B: %v", err)
		}
		got, err := CanonicalRoot(loopA)
		if err == nil || got != "" {
			t.Fatalf("CanonicalRoot(ELOOP) = %q, %v; want error not identity", got, err)
		}
		if !errors.Is(err, syscall.ELOOP) && !strings.Contains(strings.ToLower(err.Error()), "too many links") {
			t.Fatalf("CanonicalRoot(ELOOP) error = %v, want ELOOP", err)
		}
		absLoop, _ := filepath.Abs(loopA)
		if got == absLoop || got == loopA {
			t.Fatalf("CanonicalRoot(ELOOP) minted lexical identity %q", got)
		}

		denied := filepath.Join(dir, "denied")
		if err := os.Mkdir(denied, 0o700); err != nil {
			t.Fatalf("Mkdir denied: %v", err)
		}
		child := filepath.Join(denied, "root")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatalf("Mkdir child: %v", err)
		}
		if err := os.Chmod(denied, 0); err != nil {
			t.Fatalf("Chmod denied: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(denied, 0o700) })
		got, err = CanonicalRoot(child)
		_ = os.Chmod(denied, 0o700)
		if err == nil || got != "" {
			t.Fatalf("CanonicalRoot(EACCES) = %q, %v; want error not identity", got, err)
		}
		if !errors.Is(err, syscall.EACCES) && !errors.Is(err, os.ErrPermission) {
			t.Fatalf("CanonicalRoot(EACCES) error = %v, want EACCES", err)
		}
	})
}

func TestStoreRefusesLockReplacementAtAcquisitionSeam(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Store) (error, bool)
	}{
		{
			name: "ordinary mutation lock",
			run: func(store *Store) (error, bool) {
				_, err := store.Upsert(Entry{Root: "/tmp/seam", Agent: "codex", Adapter: "file", Target: "/tmp/seam"})
				return err, false
			},
		},
		{
			name: "registration lock",
			run: func(store *Store) (error, bool) {
				called := false
				err := store.WithRegistrationLock(func() error {
					called = true
					return nil
				})
				return err, called
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := New(registryTestPath(t))
			replaced := false
			previous := afterRegistryLockAcquired
			t.Cleanup(func() { afterRegistryLockAcquired = previous })
			afterRegistryLockAcquired = func(_ *os.File, lockPath string) error {
				if replaced {
					t.Fatal("lock replacement seam called more than once")
				}
				replaced = true
				if err := os.Remove(lockPath); err != nil {
					return err
				}
				return os.WriteFile(lockPath, []byte("replacement\n"), 0o600)
			}

			err, called := tt.run(store)
			if err == nil {
				t.Fatal("operation error = nil, want refusal after lock replacement")
			}
			if called {
				t.Fatal("registration callback ran after lock replacement")
			}
			if !replaced {
				t.Fatal("lock replacement seam was not called")
			}
		})
	}
}

func writeRawRegistry(t *testing.T, path string, entries []Entry) {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("Chmod registry dir: %v", err)
	}
	data, err := json.Marshal(File{SchemaVersion: SchemaVersion, Entries: entries})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
