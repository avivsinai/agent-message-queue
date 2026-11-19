package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// serviceSandbox gives a test a temporary HOME, an installed-looking
// amq-remote outside the temporary directory, and a service manager that
// records its calls and moves from no job loaded to running after a start.
func serviceSandbox(t *testing.T) *[]string {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the login service is supported on macOS and Linux")
	}
	base := canonicalTempDir(t)
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	exe := filepath.Join(base, "bin", "amq-remote")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	calls := &[]string{}
	savedRun, savedRunOutput, savedExe := serviceRun, serviceRunOutput, serviceExecutable
	t.Cleanup(func() { serviceRun, serviceRunOutput, serviceExecutable = savedRun, savedRunOutput, savedExe })
	serviceExecutable = func() (string, error) { return exe, nil }
	// TestMain installs a refusing serviceRun guard; restore the delegating
	// default so every service manager call flows through the recorded
	// serviceRunOutput stub below.
	serviceRun = func(name string, args ...string) error {
		_, err := serviceRunOutput(name, args...)
		return err
	}
	savedSleep, savedNow := serviceStartSleep, serviceStartNow
	t.Cleanup(func() { serviceStartSleep, serviceStartNow = savedSleep, savedNow })
	now := time.Now()
	serviceStartNow = func() time.Time { return now }
	serviceStartSleep = func(d time.Duration) { now = now.Add(d) }
	started := false
	serviceRunOutput = func(name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		*calls = append(*calls, call)
		if call == "launchctl print "+launchdTarget() {
			if started {
				return "\tstate = running\n\tpid = 7337\n", nil
			}
			return "", errors.New("not loaded")
		}
		if strings.HasPrefix(call, "launchctl bootstrap ") || call == "systemctl --user restart "+loginservice.Unit {
			started = true
		}
		return "", nil
	}
	return calls
}

// Bead agent-message-queue-ddm2: during a real upgrade on 2026-11-18,
// launchd loaded the job as not running while install claimed it started.
// The command sees that state at its CLI boundary and retries once.
func TestServiceInstallClaimsStartedOnlyWhenItSeesTheJobRun(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchctl shape")
	}
	for _, tc := range []struct {
		name     string
		recover  bool
		wantCode int
	}{
		{name: "loaded job stays stopped", wantCode: protocol.ExitActionRequired},
		{name: "kickstart makes job run", recover: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := serviceSandbox(t)
			kickstarts := 0
			bootstrapped := false
			serviceRunOutput = func(name string, args ...string) (string, error) {
				call := name + " " + strings.Join(args, " ")
				*calls = append(*calls, call)
				if call == "launchctl print "+launchdTarget() {
					if !bootstrapped {
						return "", errors.New("not loaded")
					}
					if tc.recover && kickstarts > 0 {
						return "\tstate = running\n\tpid = 4242\n", nil
					}
					return "\tstate = not running\n", nil
				}
				if strings.HasPrefix(call, "launchctl bootstrap ") {
					bootstrapped = true
				}
				if call == "launchctl kickstart "+launchdTarget() {
					kickstarts++
				}
				return "", nil
			}
			var stdout, stderr bytes.Buffer
			code, err := serviceInstall(canonicalTempDir(t), filepath.Join(t.TempDir(), "agent.plist"), false, &stdout)
			if err != nil {
				fmt.Fprint(&stderr, err)
			}
			if code != tc.wantCode {
				t.Fatalf("install exit = %d, want %d; %s %s", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if kickstarts != 1 {
				t.Fatalf("kickstarts = %d, want exactly one; calls %v", kickstarts, *calls)
			}
			if tc.recover {
				if err != nil || !strings.Contains(stdout.String(), "Installed and started") {
					t.Fatalf("successful install error = %v; stdout = %q", err, stdout.String())
				}
			} else {
				if !strings.Contains(stderr.String(), "state = not running") || !strings.Contains(stderr.String(), "launchctl kickstart") {
					t.Fatalf("failure does not name the state and start command: %q", stderr.String())
				}
				if strings.Contains(stdout.String(), "Installed and started") {
					t.Fatalf("a stopped service printed a start claim: %q", stdout.String())
				}
			}
		})
	}
}

// Issue #1064: after a reboot nothing restarted the endpoint, so a linked
// server showed the machine offline. service install writes a login unit
// that runs amq-remote up for the root and starts it; uninstall removes it.
func TestServiceInstallWritesALoginUnitForTheRoot(t *testing.T) {
	calls := serviceSandbox(t)
	root := canonicalTempDir(t)
	var out bytes.Buffer
	if code := run([]string{"service", "install", "--root", root}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("service install exit %d: %s", code, out.String())
	}
	path, err := loginservice.Path()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unit, want, start := string(raw), "<string>up</string>\n\t\t<string>--root</string>\n\t\t<string>"+root+"</string>", "launchctl bootstrap"
	if runtime.GOOS == "linux" {
		want, start = ` up --root "`+root+`"`, "systemctl --user restart "+loginservice.Unit
	}
	if !strings.Contains(unit, loginservice.Marker) || !strings.Contains(unit, want) {
		t.Fatalf("unit %s:\n%s\nwant it to run up --root %s", path, unit, root)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), start) {
		t.Fatalf("service manager calls %q; want %q", *calls, start)
	}
	if code := run([]string{"service", "uninstall"}, nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("service uninstall exit %d", code)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit %s still present after uninstall: %v", path, err)
	}
}

// Issue #1064: doctor names service install when the root has a link and no
// login service, as advice that never fails doctor.
func TestDoctorNotesAMissingServiceForALinkedRoot(t *testing.T) {
	serviceSandbox(t)
	root := canonicalTempDir(t)
	stateDir := filepath.Join(root, stateDirName)
	if err := manifest.Write(manifest.DefaultPath(stateDir), manifest.File{
		SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
		Links: []manifest.Link{{Name: "srv", URL: "wss://example.invalid/link"}},
	}); err != nil {
		t.Fatal(err)
	}
	out, _, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	report := out.(map[string]any)
	notes, _ := report["notes"].([]boundaryFailure)
	noted := false
	for _, n := range notes {
		noted = noted || n.Boundary == "service" && strings.Contains(n.Remedy, "service install --root "+root)
	}
	for _, f := range report["failing"].([]boundaryFailure) {
		if f.Boundary == "service" {
			t.Fatalf("doctor failed on the service: %+v", f)
		}
	}
	if !noted {
		t.Fatalf("doctor notes %+v; want a service note naming service install --root %s", notes, root)
	}
}

// Bead agent-message-queue-3qp: after a graceful stop, launchctl print
// reported the loaded job as "state = not running" with no pid and still
// exited 0, so service status said running: true for a stopped endpoint.
// The status face is the keeper: the observed stopped row and the running
// happy path, each through text and JSON, with no real service manager.
func TestServiceStatusReportsAStoppedLoadedJobAsNotRunning(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchctl shape")
	}
	cases := []struct {
		name        string
		printOut    string
		wantRunning bool
	}{
		{"stopped loaded job as observed", "\tstate = not running\n", false},
		{"running job", "\tstate = running\n\tpid = 87371\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := serviceSandbox(t)
			root := canonicalTempDir(t)
			if code := run([]string{"service", "install", "--root", root}, nil, io.Discard, io.Discard); code != 0 {
				t.Fatalf("service install exit %d", code)
			}
			savedOutput := serviceRunOutput
			t.Cleanup(func() { serviceRunOutput = savedOutput })
			serviceRunOutput = func(name string, args ...string) (string, error) {
				call := name + " " + strings.Join(args, " ")
				*calls = append(*calls, call)
				if call == "launchctl print "+launchdTarget() {
					return tc.printOut, nil
				}
				return "", nil
			}
			var text bytes.Buffer
			if code := run([]string{"service", "status", "--root", root}, nil, &text, io.Discard); code != 0 {
				t.Fatalf("service status exit %d: %s", code, text.String())
			}
			want := "(running: false)"
			if tc.wantRunning {
				want = "(running: true)"
			}
			if !strings.Contains(text.String(), want) {
				t.Fatalf("service status text %q; want it to contain %q", text.String(), want)
			}
			var jsonOut bytes.Buffer
			if code := run([]string{"service", "status", "--root", root, "--json"}, nil, &jsonOut, io.Discard); code != 0 {
				t.Fatalf("service status --json exit %d: %s", code, jsonOut.String())
			}
			wantJSON := `"running": false`
			if tc.wantRunning {
				wantJSON = `"running": true`
			}
			if !strings.Contains(jsonOut.String(), wantJSON) {
				t.Fatalf("service status JSON %s; want %s", jsonOut.String(), wantJSON)
			}
		})
	}
}
