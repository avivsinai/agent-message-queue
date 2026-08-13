package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func openTestRoot(t *testing.T) (string, *fsq.DeliveryRoot) {
	t.Helper()
	dir := t.TempDir()
	identity, err := fsq.SnapshotDeliveryRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

func validBinding() BindingRecord {
	return BindingRecord{
		Version: BindingVersion, Backend: "tmux", HostIdentity: "host:one",
		InstanceIdentity: "tmux-server:one", Profile: "tmux/darwin/v1", LaunchNonce: "nonce-one",
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: []ResourceIdentity{
			{OpaqueID: "session:amq-project-collab"}, {OpaqueID: "pane:%1", Agent: "claude"},
		}},
	}
}

func TestBindingAtomicRoundTripAndMode(t *testing.T) {
	dir, root := openTestRoot(t)
	wantPath := filepath.Join(dir, "meta", "launch", "binding.json")
	if got := BindingPath(dir); got != wantPath {
		t.Fatalf("BindingPath = %s, want core-owned %s", got, wantPath)
	}
	record := validBinding()
	if err := WriteBinding(root, record); err != nil {
		t.Fatal(err)
	}
	got, err := LoadBinding(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Backend != record.Backend || got.Resources.Resources[1].Agent != "claude" {
		t.Fatalf("binding round trip = %#v", got)
	}
	info, err := os.Stat(BindingPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("binding mode = %04o, want 0600", info.Mode().Perm())
	}

	record.LaunchNonce = "nonce-two"
	if err := WriteBinding(root, record); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(BindingPath(dir)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != bindingFilename {
		t.Fatalf("atomic replace left unexpected entries: %v", entries)
	}
}

func TestLoadBindingFailsClosed(t *testing.T) {
	tests := []struct{ name, data, want string }{
		{"malformed", `{`, "decode launch binding"},
		{"downgrade", `{"version":0}`, "unsupported binding version 0"},
		{"unknown field", `{"version":1,"backend":"x","host_identity":"h","instance_identity":"i","profile":"p","launch_nonce":"n","resources":{"version":1,"resources":[]},"extra":true}`, "unknown field"},
		{"resource downgrade", `{"version":1,"backend":"x","host_identity":"h","instance_identity":"i","profile":"p","launch_nonce":"n","resources":{"version":0,"resources":[]}}`, "unsupported resource identity set version 0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, root := openTestRoot(t)
			path := BindingPath(dir)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadBinding(root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadBinding error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadBindingRejectsPermissiveOrSymlinkState(t *testing.T) {
	dir, root := openTestRoot(t)
	path := BindingPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"version":1,"backend":"x","host_identity":"h","instance_identity":"i","profile":"p","launch_nonce":"n","resources":{"version":1,"resources":[]}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBinding(root); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("permissive LoadBinding error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(outside, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBinding(root); err == nil {
		t.Fatal("LoadBinding followed a symlink")
	}
}
