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
// Stop and PermissionRequest hook gates, so every event of every Claude
// session started amq-remote (perf audit 2026-11-05). A live binding beside
// them still starts it. Review of the gate: under zsh an empty directory
// failed the unmatched glob with an error.
func TestHookGatesStartNothingForTombstonesOnly(t *testing.T) {
	shells := []string{"/bin/sh"}
	if zsh, err := exec.LookPath("zsh"); err == nil {
		shells = append(shells, zsh)
	}
	gates := map[string]func(home, bin string) string{"stop": stopHookCommand, "permission": permissionHookCommand}
	for _, shell := range shells {
		for gate, command := range gates {
			for _, tc := range []struct {
				name  string
				files []string
				start bool
			}{
				{"empty directory", nil, false},
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
				cmd := exec.Command(shell, "-c", command(home, fake))
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "RAN=" + ran}
				out, err := cmd.CombinedOutput()
				if _, statErr := os.Stat(ran); (statErr == nil) != tc.start {
					t.Errorf("%s %s %s: started amq-remote = %v, want %v", shell, gate, tc.name, statErr == nil, tc.start)
				}
				if !tc.start && (err != nil || len(out) != 0) {
					t.Errorf("%s %s %s: %v: %q, want a quiet exit 0", shell, gate, tc.name, err, out)
				}
			}
		}
	}
}
