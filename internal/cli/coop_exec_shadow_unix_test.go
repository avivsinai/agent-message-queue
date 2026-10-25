//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Field report: launching Codex selected an older PATH installation that
// rejected the configured model. Doctor named the files, but launch was silent.
func TestCoopExecWarnsWhenPATHSelectsOlderHarness(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, version := range []string{"0.100.0", "0.101.0"} {
		dir := filepath.Join(t.TempDir(), "bin")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "codex")
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'codex-cli %s\\n'; else exit 1; fi\n", version)
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	t.Setenv("PATH", filepath.Dir(paths[0])+string(os.PathListSeparator)+filepath.Dir(paths[1]))
	sentinel := errors.New("exec sentinel")
	oldExec := coopExecProcess
	t.Cleanup(func() { coopExecProcess = oldExec })
	var selected string
	coopExecProcess = func(path string, _ []string, _ []string) error {
		selected = path
		return sentinel
	}
	output := captureWakeStderr(t, func() {
		err := runCoopExec([]string{"--root", root, "--me", "coder", "--no-wake", "--named=false", "codex"})
		if !errors.Is(err, sentinel) {
			t.Fatalf("coop exec error = %v, want exec sentinel", err)
		}
	})
	want := fmt.Sprintf("warning: PATH selects codex %q (0.100.0); newer %q (0.101.0) is later in PATH; put the intended installation first in PATH\n", paths[0], paths[1])
	if !strings.Contains(output, want) || strings.Count(output, "warning: PATH selects") != 1 {
		t.Fatalf("launch output = %q, want one %q", output, want)
	}
	if selected != paths[0] {
		t.Fatalf("executed %q, want selected installation %q", selected, paths[0])
	}
}
