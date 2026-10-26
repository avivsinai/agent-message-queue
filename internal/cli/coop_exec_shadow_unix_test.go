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
	"github.com/creack/pty"
)

// Field report: launching Codex selected an older PATH installation that
// rejected the configured model. Doctor named the files, but launch was silent.
func TestCoopExecWarnsWhenPATHSelectsOlderHarness(t *testing.T) {
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	stdin, stdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = terminal, terminal
	t.Cleanup(func() {
		os.Stdin, os.Stdout = stdin, stdout
		_ = terminal.Close()
		_ = master.Close()
	})
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

// Field report agent-message-queue-ixq (terminal launch diagnostics): redirected
// interactive Codex stdout failed after AMQ startup. The exec subcommand needs pipes.
func TestCoopExecChecksCodexTerminalsBeforeStartup(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		// refusal is the usage error text, or "" for a launch that runs.
		refusal string
		// dumb runs the launch with TERM=dumb.
		dumb bool
	}{
		{"interactive stdout redirected", []string{"-m", "example-model", "explain this project"}, "missing: stdout", false},
		{"exec stdout redirected", []string{"exec", "explain this project"}, "", false},
		// Pre-merge review of fix/wake-holds-0113 (Pro finding 5): with
		// TERM=dumb Codex asks "Continue anyway? [y/N]" before its TUI
		// starts, and a doorbell's Enter answers No.
		{"TERM=dumb resume", []string{"resume", "01a165af-5f6d-7270-bb60-671490b46ea1"}, "TERM=dumb", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, terminal, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			output, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
			if err != nil {
				_ = terminal.Close()
				_ = master.Close()
				t.Fatal(err)
			}
			stdin, stdout := os.Stdin, os.Stdout
			os.Stdin, os.Stdout = terminal, output
			t.Cleanup(func() {
				os.Stdin, os.Stdout = stdin, stdout
				_ = output.Close()
				_ = terminal.Close()
				_ = master.Close()
			})
			bin := t.TempDir()
			binary := filepath.Join(bin, "codex")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			if tc.dumb {
				for _, key := range []string{"TERM_PROGRAM", "GHOSTTY_RESOURCES_DIR", "WEZTERM_VERSION", "ITERM_SESSION_ID", "ITERM_PROFILE", "ITERM_PROFILE_NAME",
					"KITTY_WINDOW_ID", "ALACRITTY_SOCKET", "KONSOLE_VERSION", "GNOME_TERMINAL_SCREEN", "VTE_VERSION", "WT_SESSION"} {
					t.Setenv(key, "")
					_ = os.Unsetenv(key)
				}
				t.Setenv("TERM", "dumb")
			}
			root := filepath.Join(secureTempDirForTest(t), "queue")
			sentinel := errors.New("exec sentinel")
			oldExec := coopExecProcess
			t.Cleanup(func() { coopExecProcess = oldExec })
			var selected string
			var argv []string
			coopExecProcess = func(path string, args, _ []string) error {
				selected, argv = path, append([]string(nil), args...)
				return sentinel
			}
			args := append([]string{"--root", root, "--me", "coder", "--no-wake", "--named=false", "-y", "codex", "--"}, tc.args...)
			err = runCoopExec(args)
			if tc.refusal != "" {
				if GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("Codex launch error = %v, want usage error naming %s", err, tc.refusal)
				}
				if selected != "" {
					t.Fatalf("interactive Codex reached exec: %q", selected)
				}
				if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("interactive Codex provisioned its queue: %v", err)
				}
				return
			}
			if !errors.Is(err, sentinel) || selected != binary || len(argv) < len(tc.args) {
				t.Fatalf("noninteractive Codex: error=%v selected=%q argv=%q", err, selected, argv)
			}
			if got := argv[len(argv)-len(tc.args):]; strings.Join(got, "\x00") != strings.Join(tc.args, "\x00") {
				t.Fatalf("noninteractive arguments = %q, want %q", got, tc.args)
			}
		})
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
