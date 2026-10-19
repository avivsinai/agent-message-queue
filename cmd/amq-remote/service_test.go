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
	savedRun, savedExe := serviceRun, serviceExecutable
	t.Cleanup(func() { serviceRun, serviceExecutable = savedRun, savedExe })
	serviceExecutable = func() (string, error) { return exe, nil }
	serviceRun = func(name string, args ...string) error {
		call := name + " " + strings.Join(args, " ")
		*calls = append(*calls, call)
		if call == "launchctl print "+launchdTarget() {
			return errors.New("not loaded")
		}
		return nil
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
