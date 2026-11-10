package execidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// A current marker written by an AMQ before markers were keyed by the
// persisted root key still marks its process current (audit 2026-11-10).
func TestListReadsCurrentMarkerUnderLegacyKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	r := Record{
		Harness: launch.ClaudeProvider, Holder: launch.HolderIdentity{PID: 4242, ProcessStart: "start", BootID: "boot"},
		StartUnix: 1, Root: filepath.Join(home, "root"), BaseRoot: filepath.Join(home, "root"), Me: "alpha",
		RootID: "v1:abc:1:2", BaseRootID: "v1:abc:1:2",
	}
	if err := Publish(r); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(home, ".amq", storeName, "current")
	sum := sha256.Sum256([]byte(r.RootID + "\x00" + r.Me))
	if err := os.Rename(filepath.Join(current, currentKey(r)+".json"), filepath.Join(current, hex.EncodeToString(sum[:16])+".json")); err != nil {
		t.Fatal(err)
	}
	entries, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Current {
		t.Fatalf("entries = %+v, want one current entry", entries)
	}
}
