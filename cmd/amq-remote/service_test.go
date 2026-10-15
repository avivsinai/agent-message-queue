package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

// Issue #1064: after a reboot nothing restarted the endpoint, so a linked
// server showed the machine offline. service install writes a login unit
// that runs amq-remote up for the root and starts it; uninstall removes it.
func TestServiceInstallWritesALoginUnitForTheRoot(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the login service is supported on macOS and Linux")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	root := canonicalTempDir(t)
	var calls []string
	saved := serviceRun
	t.Cleanup(func() { serviceRun = saved })
	serviceRun = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	var out bytes.Buffer
	if code := run([]string{"service", "install", "--root", root}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("service install exit %d: %s", code, out.String())
	}
	path, err := servicePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unit, want, start := string(raw), "<string>up</string>\n\t\t<string>--root</string>\n\t\t<string>"+root+"</string>", "launchctl bootstrap"
	if runtime.GOOS == "linux" {
		want, start = ` up --root "`+root+`"`, "systemctl --user enable --now "+serviceUnit
	}
	if !strings.Contains(unit, serviceMarker) || !strings.Contains(unit, want) {
		t.Fatalf("unit %s:\n%s\nwant it to run up --root %s", path, unit, root)
	}
	if !strings.Contains(strings.Join(calls, "\n"), start) {
		t.Fatalf("service manager calls %q; want %q", calls, start)
	}
	if code := run([]string{"service", "uninstall"}, nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("service uninstall exit %d", code)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit %s still present after uninstall: %v", path, err)
	}
}
