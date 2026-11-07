//go:build darwin || linux

package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
)

// The defect lives in the installed shell, so the test runs it. A bindings
// directory that holds only the tombstones of removed bindings opened the
// Stop hook gate, so every Stop of every Claude session started amq-remote
// (perf audit 2026-11-05). A live binding beside them still starts it.
func TestStopHookGateStartsNothingForTombstonesOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		start bool
	}{
		{"tombstones only", []string{"a.ended.json", "b.ended.json"}, false},
		{"live binding", []string{"a.ended.json", "b.json", "c.ended.json"}, true},
	} {
		home := t.TempDir()
		ran := filepath.Join(home, "ran")
		fake := filepath.Join(home, "amq-remote")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch \"$RAN\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		bindings := filepath.Join(amqhome.Under(home), "remote", "bindings")
		if err := os.MkdirAll(bindings, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range tc.files {
			if err := os.WriteFile(filepath.Join(bindings, name), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("/bin/sh", "-c", stopHookCommand(home, fake))
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "RAN=" + ran}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", tc.name, err, out)
		}
		if _, err := os.Stat(ran); (err == nil) != tc.start {
			t.Errorf("%s: started amq-remote = %v, want %v", tc.name, err == nil, tc.start)
		}
	}
}
