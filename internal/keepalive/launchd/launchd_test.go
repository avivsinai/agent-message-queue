package launchd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDefaultLabelIsProjectOwned(t *testing.T) {
	const want = "io.github.avivsinai.amq-keepalive"
	if DefaultLabel != want {
		t.Fatalf("DefaultLabel = %q, want %q", DefaultLabel, want)
	}
}

func TestBuildPlistContainsSupervisorContract(t *testing.T) {
	opts := Options{
		Label:        "com.example.amq-keepalive",
		BinaryPath:   "/usr/local/bin/amq-keepalive",
		RegistryPath: "/Users/test/.amq-keepalive/registry.json",
		AMQPath:      "/opt/homebrew/bin/amq",
		Interval:     15 * time.Second,
		StdoutPath:   "/Users/test/Library/Logs/amq-keepalive/out.log",
		StderrPath:   "/Users/test/Library/Logs/amq-keepalive/err.log",
	}
	plist := BuildPlist(opts)

	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.example.amq-keepalive</string>",
		"<key>ProgramArguments</key>",
		"<string>/usr/local/bin/amq-keepalive</string>",
		"<string>supervise</string>",
		"<string>--registry</string>",
		"<string>/Users/test/.amq-keepalive/registry.json</string>",
		"<string>--amq</string>",
		"<string>/opt/homebrew/bin/amq</string>",
		"<string>--self</string>",
		"<string>--interval</string>",
		"<string>15s</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"/Applications/cmux.app/Contents/Resources/bin:/opt/homebrew/bin",
	} {
		if !bytes.Contains(plist, []byte(want)) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestInstallNoLoadWritesPlist(t *testing.T) {
	dir := t.TempDir()
	plistPath := filepath.Join(dir, "LaunchAgents", "com.example.amq-keepalive.plist")
	opts := Options{
		Label:        "com.example.amq-keepalive",
		PlistPath:    plistPath,
		BinaryPath:   "/bin/echo",
		RegistryPath: filepath.Join(dir, "registry.json"),
		AMQPath:      "/bin/echo",
		Interval:     time.Second,
		Load:         false,
	}
	if err := Install(context.Background(), opts); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Contains(data, []byte("<string>supervise</string>")) {
		t.Fatalf("plist missing supervise command:\n%s", data)
	}
	info, err := os.Stat(plistPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("plist mode = %v, want 0644", got)
	}
}

func TestUninstallNoUnloadRemovesPlist(t *testing.T) {
	plistPath := filepath.Join(t.TempDir(), "com.example.amq-keepalive.plist")
	data := BuildPlist(Options{
		Label:        "com.example.amq-keepalive",
		BinaryPath:   "/bin/echo",
		RegistryPath: "/tmp/registry.json",
		AMQPath:      "/bin/echo",
		Interval:     time.Second,
		StdoutPath:   "/tmp/out.log",
		StderrPath:   "/tmp/err.log",
	})
	if err := os.WriteFile(plistPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := Uninstall(context.Background(), "com.example.amq-keepalive", plistPath, false); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("Stat() error = %v, want not exist", err)
	}
}

func TestUninstallBootoutFailurePreservesPlist(t *testing.T) {
	dir := t.TempDir()
	plistPath := filepath.Join(dir, "com.example.amq-keepalive.plist")
	data := BuildPlist(Options{
		Label:        "com.example.amq-keepalive",
		BinaryPath:   "/bin/echo",
		RegistryPath: "/tmp/registry.json",
		AMQPath:      "/bin/echo",
		Interval:     time.Second,
		StdoutPath:   "/tmp/out.log",
		StderrPath:   "/tmp/err.log",
	})
	if err := os.WriteFile(plistPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	launchctl := filepath.Join(binDir, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\necho bootout failed >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := Uninstall(context.Background(), "com.example.amq-keepalive", plistPath, true)
	if err == nil {
		t.Fatal("Uninstall() error = nil, want bootout failure")
	}
	if !strings.Contains(err.Error(), "launchctl bootout") {
		t.Fatalf("Uninstall() error = %v, want bootout context", err)
	}
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("ReadFile() after failed bootout error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("plist changed after failed bootout:\n%s", got)
	}
}

func TestInstallRefusesForeignPlist(t *testing.T) {
	dir := t.TempDir()
	plistPath := filepath.Join(dir, "LaunchAgents", "com.example.amq-keepalive.plist")
	foreign := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.example.amq-keepalive</string>
	<key>ProgramArguments</key>
	<array><string>/usr/bin/true</string></array>
</dict>
</plist>
`)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(plistPath, foreign, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	opts := Options{
		Label:        "com.example.amq-keepalive",
		PlistPath:    plistPath,
		BinaryPath:   "/bin/echo",
		RegistryPath: filepath.Join(dir, "registry.json"),
		AMQPath:      "/bin/echo",
		Interval:     time.Second,
		Load:         false,
	}
	if err := Install(context.Background(), opts); err == nil {
		t.Fatal("Install() error = nil, want foreign plist refusal")
	}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(data, foreign) {
		t.Fatalf("foreign plist was modified:\n%s", data)
	}
}

func TestUninstallRefusesForeignPlist(t *testing.T) {
	plistPath := filepath.Join(t.TempDir(), "com.example.amq-keepalive.plist")
	foreign := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.example.amq-keepalive</string>
	<key>ProgramArguments</key>
	<array><string>/usr/bin/true</string></array>
</dict>
</plist>
`)
	if err := os.WriteFile(plistPath, foreign, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := Uninstall(context.Background(), "com.example.amq-keepalive", plistPath, false); err == nil {
		t.Fatal("Uninstall() error = nil, want foreign plist refusal")
	}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(data, foreign) {
		t.Fatalf("foreign plist was modified:\n%s", data)
	}
}

func TestInstallAndUninstallRequireEveryOwnershipMarker(t *testing.T) {
	const label = "com.example.amq-keepalive"
	base := Options{
		Label:        label,
		BinaryPath:   "/bin/echo",
		RegistryPath: "/tmp/registry.json",
		AMQPath:      "/bin/echo",
		Interval:     time.Second,
		StdoutPath:   "/tmp/out.log",
		StderrPath:   "/tmp/err.log",
	}
	owned := BuildPlist(base)
	markers := []string{
		"<key>Label</key>",
		"<string>" + label + "</string>",
		"<key>ProgramArguments</key>",
		"<string>supervise</string>",
		"<string>--registry</string>",
		"<string>--amq</string>",
		"<string>--self</string>",
	}
	for _, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			foreign := bytes.Replace(owned, []byte(marker), nil, 1)
			if bytes.Equal(foreign, owned) {
				t.Fatalf("marker %q was not present in owned plist", marker)
			}

			installPath := filepath.Join(t.TempDir(), "install.plist")
			if err := os.WriteFile(installPath, foreign, 0o644); err != nil {
				t.Fatalf("WriteFile install fixture: %v", err)
			}
			installOpts := base
			installOpts.PlistPath = installPath
			if err := Install(context.Background(), installOpts); err == nil {
				t.Fatalf("Install accepted plist missing %q", marker)
			}
			got, err := os.ReadFile(installPath)
			if err != nil || !bytes.Equal(got, foreign) {
				t.Fatalf("Install changed foreign plist: err=%v bytes=%q", err, got)
			}

			uninstallPath := filepath.Join(t.TempDir(), "uninstall.plist")
			if err := os.WriteFile(uninstallPath, foreign, 0o644); err != nil {
				t.Fatalf("WriteFile uninstall fixture: %v", err)
			}
			if err := Uninstall(context.Background(), label, uninstallPath, false); err == nil {
				t.Fatalf("Uninstall accepted plist missing %q", marker)
			}
			got, err = os.ReadFile(uninstallPath)
			if err != nil || !bytes.Equal(got, foreign) {
				t.Fatalf("Uninstall changed foreign plist: err=%v bytes=%q", err, got)
			}
		})
	}
}

func TestNormalizeOptionsFillsIndependentDefaults(t *testing.T) {
	dir := t.TempDir()
	amqPath := filepath.Join(dir, "amq")
	if err := os.WriteFile(amqPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile amq: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	normalized, err := NormalizeOptions(Options{
		BinaryPath:   "/bin/echo",
		RegistryPath: filepath.Join(dir, "registry.json"),
		StdoutPath:   filepath.Join(dir, "explicit.out"),
	})
	if err != nil {
		t.Fatalf("NormalizeOptions: %v", err)
	}
	if normalized.Label != DefaultLabel || normalized.AMQPath != amqPath || normalized.Interval != time.Minute {
		t.Fatalf("defaults = %+v", normalized)
	}
	if normalized.StdoutPath != filepath.Join(dir, "explicit.out") || normalized.StderrPath == "" {
		t.Fatalf("log defaults = stdout %q stderr %q", normalized.StdoutPath, normalized.StderrPath)
	}

	explicitStderr := filepath.Join(dir, "explicit.err")
	normalized, err = NormalizeOptions(Options{
		BinaryPath:   "/bin/echo",
		RegistryPath: filepath.Join(dir, "registry.json"),
		AMQPath:      amqPath,
		StderrPath:   explicitStderr,
	})
	if err != nil {
		t.Fatalf("NormalizeOptions partial logs: %v", err)
	}
	if normalized.StdoutPath == "" || normalized.StderrPath != explicitStderr {
		t.Fatalf("partial log defaults = stdout %q stderr %q", normalized.StdoutPath, normalized.StderrPath)
	}
}

func TestServiceTargetUsesLaunchdLabelTarget(t *testing.T) {
	want := "gui/" + strconv.Itoa(os.Getuid()) + "/com.example.amq-keepalive"
	if got := serviceTarget("com.example.amq-keepalive"); got != want {
		t.Fatalf("serviceTarget() = %q, want %q", got, want)
	}
}
