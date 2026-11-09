//go:build !windows

package codexidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Review of #993 (Pro, P1): the first publication created the store's
// directories without syncing them into their parents, so a crash could
// lose a published thread and its commands would fall back to default
// identity. Round 2: a retry after a failed sync skipped it, because the
// directory then existed. Every publication syncs the store's directories
// into their parents, so a sync that keeps failing keeps failing it.
func TestPublishSyncsTheDirectoriesItCreates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var synced []string
	restore := fsq.SetPackageSyncDirFaultForTest(func(dir string) error {
		synced = append(synced, dir)
		if dir == home {
			return errors.New("injected sync failure")
		}
		return nil
	})
	defer restore()
	rec := Record{Thread: "01a1166e-dd8e-77c0-bc3b-a4b7e24e91ab", CodexHome: filepath.Join(home, ".codex"),
		Root: filepath.Join(home, "q"), BaseRoot: filepath.Join(home, "q"), Me: "codex", RootID: "r", BaseRootID: "b"}
	if err := Publish(rec); err == nil {
		t.Fatal("publication succeeded although the new store directory could not be synced into the home")
	}
	if err := Publish(rec); err == nil {
		t.Fatal("a retry succeeded although the home still could not be synced")
	}
	if len(synced) == 0 || synced[0] != home {
		t.Fatalf("synced %v, want the home synced first", synced)
	}
	if _, err := os.Stat(filepath.Join(home, ".amq", "codex-threads", "threads", rec.Thread+".json")); !os.IsNotExist(err) {
		t.Fatalf("a record was written before the store was durable: %v", err)
	}
}

// Field regression (2026-11-09): a macOS reboot renumbers st_dev of the
// root, so a coop exec after it named the handle's current thread by a new
// key and left the thread from before the reboot carrying the identity. A
// marker an earlier AMQ named by the raw token still resolves.
func TestCurrentThreadSurvivesReboot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "q")
	rec := func(thread, rootID string) Record {
		return Record{Thread: thread, CodexHome: filepath.Join(home, ".codex"), Root: root, BaseRoot: root, Me: "codex", RootID: rootID, BaseRootID: rootID}
	}
	before := rec("01a1166e-dd8e-77c0-bc3b-a4b7e24e91c1", "v1:darwin:100000d:278027")
	after := rec("01a1166e-dd8e-77c0-bc3b-a4b7e24e91c2", "v1:darwin:1000011:278027")
	for _, r := range []Record{before, after} {
		if err := Publish(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Lookup(before.Thread, before.CodexHome); err == nil {
		t.Fatal("the thread from before the reboot kept the identity a later coop exec took")
	}
	if _, err := Lookup(after.Thread, after.CodexHome); err != nil {
		t.Fatal(err)
	}

	legacy := rec("01a1166e-dd8e-77c0-bc3b-a4b7e24e91c3", "v1:darwin:1000011:99")
	if err := Publish(legacy); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(home, ".amq", "codex-threads", "current")
	sum := sha256.Sum256([]byte(legacy.RootID + "\x00" + legacy.Me))
	if err := os.Rename(filepath.Join(current, currentKey(legacy)+".json"), filepath.Join(current, hex.EncodeToString(sum[:16])+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup(legacy.Thread, legacy.CodexHome); err != nil {
		t.Fatalf("thread recorded by an earlier AMQ: %v", err)
	}
}
