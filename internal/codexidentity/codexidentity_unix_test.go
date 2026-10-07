//go:build !windows

package codexidentity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Review of #993 (Pro, P1): the first publication created the store's
// directories without syncing them into their parents, so a crash could
// lose a published thread and its commands would fall back to default
// identity. A directory that cannot be synced into its parent fails the
// publication.
func TestPublishSyncsTheDirectoriesItCreates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var synced []string
	restore := fsq.SyncDirAmbientSwapForTest(func(dir string) error {
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
	if len(synced) == 0 || synced[0] != home {
		t.Fatalf("synced %v, want the home synced first", synced)
	}
	if _, err := os.Stat(filepath.Join(home, ".amq", "codex-threads", "threads", rec.Thread+".json")); !os.IsNotExist(err) {
		t.Fatalf("a record was written before the store was durable: %v", err)
	}
}
