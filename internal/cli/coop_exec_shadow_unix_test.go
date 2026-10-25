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
	"github.com/avivsinai/agent-message-queue/internal/launch"
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

// Pre-merge review found that version diagnostics could run project-owned
// binaries even though managed launch refuses them as providers.
func TestManagedLaunchWarningSkipsProjectExecutables(t *testing.T) {
	project, external := t.TempDir(), t.TempDir()
	unsafe := filepath.Join(project, "bin")
	newer := filepath.Join(t.TempDir(), "bin")
	marker := filepath.Join(project, "executed")
	for _, dir := range []string{unsafe, newer} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	selected := filepath.Join(external, "codex")
	for path, script := range map[string]string{
		selected:                       "#!/bin/sh\nprintf 'codex-cli 0.100.0\\n'\n",
		filepath.Join(newer, "codex"):  "#!/bin/sh\nprintf 'codex-cli 0.101.0\\n'\n",
		filepath.Join(unsafe, "codex"): fmt.Sprintf("#!/bin/sh\nprintf ran > %s\nprintf 'codex-cli 0.102.0\\n'\n", shellQuotePosix(marker)),
	} {
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", strings.Join([]string{external, unsafe, newer}, string(os.PathListSeparator)))
	identity, err := fsq.StableFileIdentity(selected)
	if err != nil {
		t.Fatal(err)
	}
	output := captureWakeStderr(t, func() {
		warnShadowedHarness(selected, selected, &launch.ExecutionTicket{
			ProjectRoot: project, Provider: launch.CodexProvider,
			ProviderExecutable: selected, ProviderExecutableIdentity: identity,
		})
	})
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project candidate executed: %v", err)
	}
	if !strings.Contains(output, filepath.Join(newer, "codex")) || strings.Contains(output, unsafe) {
		t.Fatalf("warning = %q, want the external newer installation", output)
	}
}
