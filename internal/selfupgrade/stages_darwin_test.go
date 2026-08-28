//go:build darwin

package selfupgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
)

func TestSelfUpgradeStagePIDLiveClassifiesProcess(t *testing.T) {
	tests := []struct {
		name        string
		killErr     error
		lookPathErr error
		probeOutput string
		probeErr    error
		wantLive    bool
		wantPS      bool
	}{
		{
			name:     "eperm keeps stage",
			killErr:  syscall.EPERM,
			wantLive: true,
		},
		{
			name:        "foreign command is stale",
			probeOutput: "sleep\n",
			wantLive:    false,
			wantPS:      true,
		},
		{
			name:        "amq command keeps stage",
			probeOutput: "amq\n",
			wantLive:    true,
			wantPS:      true,
		},
		{
			name:        "keepalive command keeps stage",
			probeOutput: "amq-keepalive\n",
			wantLive:    true,
			wantPS:      true,
		},
		{
			name:     "esrch removes stage",
			killErr:  syscall.ESRCH,
			wantLive: false,
		},
		{
			name:        "missing ps keeps stage",
			lookPathErr: errors.New("ps unavailable"),
			wantLive:    true,
			wantPS:      true,
		},
		{
			name:     "probe error keeps stage",
			probeErr: errors.New("ps failed"),
			wantLive: true,
			wantPS:   true,
		},
		{
			name:        "ambiguous command keeps stage",
			probeOutput: "amq\nsleep\n",
			wantLive:    true,
			wantPS:      true,
		},
	}

	previousKill := selfUpgradeStagePIDKill
	previousToolLstat := selfUpgradeDarwinToolLstat
	previousProbe := selfUpgradeStageRunBoundedProbe
	t.Cleanup(func() {
		selfUpgradeStagePIDKill = previousKill
		selfUpgradeDarwinToolLstat = previousToolLstat
		selfUpgradeStageRunBoundedProbe = previousProbe
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const pid = 12345
			probeCalls := 0
			toolLstatCalls := 0
			selfUpgradeStagePIDKill = func(gotPID int, signal syscall.Signal) error {
				if gotPID != pid || signal != 0 {
					t.Fatalf("Kill(%d, %d), want Kill(%d, 0)", gotPID, signal, pid)
				}
				return tt.killErr
			}
			selfUpgradeDarwinToolLstat = func(path string) (os.FileInfo, error) {
				toolLstatCalls++
				if path != "/bin/ps" {
					t.Fatalf("Lstat(%q), want /bin/ps", path)
				}
				if tt.lookPathErr != nil {
					return nil, tt.lookPathErr
				}
				return os.Lstat("/bin/ps")
			}
			selfUpgradeStageRunBoundedProbe = func(
				ctx context.Context,
				path string,
				args []string,
				_ BoundedProbeOptions,
			) ([]byte, error) {
				probeCalls++
				if path != "/bin/ps" {
					t.Fatalf("probe path = %q, want /bin/ps", path)
				}
				wantArgs := []string{"-o", "comm=", "-p", strconv.Itoa(pid)}
				if !slices.Equal(args, wantArgs) {
					t.Fatalf("probe args = %#v, want %#v", args, wantArgs)
				}
				if ctx == nil {
					t.Fatal("probe context is nil")
				}
				return []byte(tt.probeOutput), tt.probeErr
			}

			got := selfUpgradeStagePIDLive(pid)
			if got != tt.wantLive {
				t.Fatalf("selfUpgradeStagePIDLive(%d) = %t, want %t", pid, got, tt.wantLive)
			}
			if got := toolLstatCalls > 0; got != tt.wantPS {
				t.Fatalf("ps validation called = %t, want %t", got, tt.wantPS)
			}
			wantProbeCalls := 0
			if tt.wantPS && tt.lookPathErr == nil {
				wantProbeCalls = 1
			}
			if probeCalls != wantProbeCalls {
				t.Fatalf("probe calls = %d, want %d", probeCalls, wantProbeCalls)
			}
		})
	}
}

func TestCleanupStagesRemovesDeadAndForeignStages(t *testing.T) {
	dir := t.TempDir()
	locator := filepath.Join(dir, "amq-keepalive")
	const (
		foreignPID = 1001
		amqPID     = 1002
		deadPID    = 1003
	)
	stageDirs := map[int]string{}
	for _, pid := range []int{foreignPID, amqPID, deadPID} {
		stageDir := filepath.Join(dir, selfUpgradeStagePrefix(locator)+strconv.Itoa(pid)+"-stage")
		stageDirs[pid] = stageDir
		if err := os.Mkdir(stageDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stageDir, filepath.Base(locator)), []byte("stage"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	previousKill := selfUpgradeStagePIDKill
	previousToolLstat := selfUpgradeDarwinToolLstat
	previousProbe := selfUpgradeStageRunBoundedProbe
	t.Cleanup(func() {
		selfUpgradeStagePIDKill = previousKill
		selfUpgradeDarwinToolLstat = previousToolLstat
		selfUpgradeStageRunBoundedProbe = previousProbe
	})
	selfUpgradeStagePIDKill = func(pid int, _ syscall.Signal) error {
		switch pid {
		case foreignPID, amqPID:
			return nil
		case deadPID:
			return syscall.ESRCH
		default:
			t.Fatalf("unexpected PID %d", pid)
			return syscall.ESRCH
		}
	}
	selfUpgradeDarwinToolLstat = func(path string) (os.FileInfo, error) {
		if path != "/bin/ps" {
			t.Fatalf("Lstat(%q), want /bin/ps", path)
		}
		return os.Lstat(path)
	}
	selfUpgradeStageRunBoundedProbe = func(
		_ context.Context,
		path string,
		args []string,
		_ BoundedProbeOptions,
	) ([]byte, error) {
		if path != "/bin/ps" {
			t.Fatalf("probe path = %q, want /bin/ps", path)
		}
		if len(args) != 4 || args[0] != "-o" || args[1] != "comm=" || args[2] != "-p" {
			t.Fatalf("probe args = %#v, want ps -o comm= -p <pid>", args)
		}
		switch args[3] {
		case strconv.Itoa(foreignPID):
			return []byte("sleep\n"), nil
		case strconv.Itoa(amqPID):
			return []byte("amq-keepalive\n"), nil
		default:
			t.Fatalf("unexpected probe PID %q", args[3])
			return nil, errors.New("unexpected probe PID")
		}
	}

	if err := CleanupStages(locator); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stageDirs[foreignPID]); !os.IsNotExist(err) {
		t.Fatalf("foreign stage = %v, want removed", err)
	}
	if _, err := os.Stat(stageDirs[deadPID]); !os.IsNotExist(err) {
		t.Fatalf("dead stage = %v, want removed", err)
	}
	if _, err := os.Stat(stageDirs[amqPID]); err != nil {
		t.Fatalf("AMQ stage = %v, want preserved", err)
	}
}
