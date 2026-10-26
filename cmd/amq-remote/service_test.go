package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
)

// serviceSandbox gives a test a temporary HOME, an installed-looking
// amq-remote outside the temporary directory, and a service manager that
// records its calls and reports no job loaded.
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
	serviceRunOutput = func(name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		*calls = append(*calls, call)
		if call == "launchctl print "+launchdTarget() {
			return "", errors.New("not loaded")
		}
		return "", nil
	}
	return calls
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
