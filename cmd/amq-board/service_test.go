package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/loginservice"
)

func TestBoardServiceUnit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix service paths")
	}
	const dir = `/var/work/board & 50%"`
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			unit := boardServiceUnit(storeDigest(filepath.Join(dir, "extensions", "board")))
			configureWatch(&unit, dir, "/opt/homebrew/bin/amq-board", "review", "lead,alt", "floor", filepath.Join(dir, "extensions", "board", "watch.log"))
			got, err := loginservice.Render(platform, unit)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "board-service", platform+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// Review found a newline in the project path could introduce a unit setting.
func TestBoardServiceRefusesMultilineDirectory(t *testing.T) {
	u := boardServiceUnit(storeDigest("/board"))
	configureWatch(&u, "/a\nb", "/bin/amq-board", "", "", "", "")
	if _, err := u.Content(); err == nil {
		t.Fatal("accepted a multiline project directory")
	}
}

type versionRunner struct {
	noGit
	exe string
}

func (r versionRunner) RunLimited(_ context.Context, _ string, argv []string, _ int) ([]byte, error) {
	if !slices.Equal(argv, []string{r.exe, "--version"}) {
		return nil, fmt.Errorf("unexpected version command %q", argv)
	}
	return []byte("1.2.3\n"), nil
}

func TestBoardServiceStatus(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix login service")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("TMPDIR", filepath.Join(dir, "tmp"))
	t.Setenv("PATH", dir)
	exe := filepath.Join(dir, "amq-board")
	if err := os.WriteFile(exe, nil, 0700); err != nil {
		t.Fatal(err)
	}
	previous, prevRun, prevOutput := serviceExecutable, loginservice.Run, loginservice.RunOutput
	t.Cleanup(func() { serviceExecutable, loginservice.Run, loginservice.RunOutput = previous, prevRun, prevOutput })
	serviceExecutable = func() (string, error) { return exe, nil }
	for _, tc := range []struct {
		name, state, version string
		running              bool
	}{
		{"missing", "missing", "", false}, {"running", "current", "1.2.3", true},
		{"old binary", "stale", "binary missing", false}, {"stopped", "current", "1.2.3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loginservice.RunOutput = func(_ string, _ ...string) (string, error) {
				if tc.running {
					return "\tstate = running\n\tpid = 73\n", nil
				}
				return "", errors.New("inactive")
			}
			loginservice.Run = func(name string, args ...string) error { _, err := loginservice.RunOutput(name, args...); return err }
			installedExe := exe
			if tc.state == "stale" {
				installedExe = filepath.Join(dir, "old", "amq-board")
			}
			notify := ""
			if tc.state != "missing" {
				notify = "lead,alt"
				u := boardServiceUnit(storeDigest(filepath.Join(dir, "extensions", "board")))
				configureWatch(&u, dir, installedExe, "", notify, "floor", filepath.Join(dir, "extensions", "board", "watch.log"))
				raw, err := u.Content()
				if err != nil {
					t.Fatal(err)
				}
				path, err := u.Path()
				if err != nil {
					t.Fatal(err)
				}
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			st, err := inspectBoardService(versionRunner{exe: exe})
			if err != nil || st.State != tc.state || st.Version != tc.version || st.Running != tc.running || st.Notify != notify ||
				(notify != "" && (st.Exe != installedExe || st.As != "floor")) {
				t.Fatalf("status = %+v, %v; want %+v", st, err, tc)
			}
		})
	}
}
