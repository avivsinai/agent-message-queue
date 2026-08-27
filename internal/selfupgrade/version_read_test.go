package selfupgrade

import (
	"context"
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"
)

func TestReadEmbeddedVersionFromReleaseBinary(t *testing.T) {
	binary := buildVersionReaderFixture(t, "1.2.3", true)

	version, err := ReadEmbeddedVersion(binary)
	if err != nil {
		t.Fatalf("ReadEmbeddedVersion() error = %v", err)
	}
	if version != "1.2.3" {
		t.Fatalf("ReadEmbeddedVersion() = %q, want 1.2.3", version)
	}

	image, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	version, err = ReadEmbeddedVersionFromOpenFile(image)
	if err != nil {
		t.Fatalf("ReadEmbeddedVersionFromOpenFile() error = %v", err)
	}
	if version != "1.2.3" {
		t.Fatalf("ReadEmbeddedVersionFromOpenFile() = %q, want 1.2.3", version)
	}
}

func TestReadEmbeddedVersionDefersTruncatedImage(t *testing.T) {
	complete := buildVersionReaderFixture(t, "1.2.3", true)
	raw, err := os.ReadFile(complete)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2 {
		t.Fatalf("version reader fixture is unexpectedly small: %d bytes", len(raw))
	}
	path := filepath.Join(t.TempDir(), "truncated")
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadEmbeddedVersion(path); err == nil {
		t.Fatal("ReadEmbeddedVersion() accepted a truncated image")
	}
}

func TestReadEmbeddedVersionDefersNonGoImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 9.9.9\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadEmbeddedVersion(path); err == nil {
		t.Fatal("ReadEmbeddedVersion() accepted a non-Go image")
	}
}

func TestReadEmbeddedVersionDefersPlainGoBuild(t *testing.T) {
	binary := buildVersionReaderFixture(t, "1.2.3", false)
	if _, err := ReadEmbeddedVersion(binary); err == nil {
		t.Fatal("ReadEmbeddedVersion() accepted a plain development build without a release version")
	}
}

func TestReadEmbeddedVersionDefersMalformedLinkerVersion(t *testing.T) {
	binary := buildVersionReaderFixture(t, "not-a-semantic-version", true)
	if _, err := ReadEmbeddedVersion(binary); err == nil {
		t.Fatal("ReadEmbeddedVersion() accepted a malformed release version")
	}
}

func TestEmbeddedVersionFromBuildInfoRequiresLinkerVersion(t *testing.T) {
	info := &buildinfo.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}
	version, ok := embeddedVersionFromBuildInfo(info)
	if ok || version != "" {
		t.Fatalf("embeddedVersionFromBuildInfo() = %q, %t; want unknown", version, ok)
	}
}

func TestEmbeddedVersionFromBuildInfoUsesLastUnambiguousAssignment(t *testing.T) {
	info := &buildinfo.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "-ldflags", Value: "-X main.version=1.0.0"},
		{Key: "-ldflags", Value: "-X main.version=1.2.3"},
	}}
	version, ok := embeddedVersionFromBuildInfo(info)
	if !ok || version != "1.2.3" {
		t.Fatalf("embeddedVersionFromBuildInfo() = %q, %t; want 1.2.3, true", version, ok)
	}

	info.Settings = append(info.Settings, debug.BuildSetting{
		Key:   "-ldflags",
		Value: "other/pkg=-X -X main.version=1.3.0",
	})
	if version, ok := embeddedVersionFromBuildInfo(info); ok || version != "" {
		t.Fatalf("embeddedVersionFromBuildInfo() with ambiguous setting = %q, %t; want unknown", version, ok)
	}
}

func TestEmbeddedVersionFromBuildInfoRejectsUnknownVersion(t *testing.T) {
	for _, info := range []*buildinfo.BuildInfo{
		nil,
		{Main: debug.Module{Version: ""}},
		{Main: debug.Module{Version: "v1.2.3"}},
		{Main: debug.Module{Version: "(devel)"}},
		{Settings: []debug.BuildSetting{{Key: "-ldflags", Value: "-X main.version="}}},
	} {
		if version, ok := embeddedVersionFromBuildInfo(info); ok || version != "" {
			t.Fatalf("embeddedVersionFromBuildInfo(%#v) = %q, %t; want unknown", info, version, ok)
		}
	}
}

func TestEmbeddedVersionFromLDFlagsParsesSupportedForms(t *testing.T) {
	for _, test := range []struct {
		flags string
		want  string
	}{
		{flags: "-s -w -X main.version=1.2.3", want: "1.2.3"},
		{flags: "-Xmain.version=1.2.3", want: "1.2.3"},
		{flags: "-X=main.version=1.2.3", want: "1.2.3"},
		{flags: "-X main.version=1.0.0 -X main.version=1.2.3", want: "1.2.3"},
		{flags: `'-X' 'main.version=1.2.3'`, want: "1.2.3"},
		{flags: `"-X=main.version=1.2.3"`, want: "1.2.3"},
	} {
		got, ok := embeddedVersionFromLDFlags(test.flags)
		if !ok || got != test.want {
			t.Errorf("embeddedVersionFromLDFlags(%q) = %q, %t; want %q, true", test.flags, got, ok, test.want)
		}
	}
	if _, ok := embeddedVersionFromLDFlags("-s -w"); ok {
		t.Fatal("embeddedVersionFromLDFlags() accepted flags without main.version")
	}
	for _, flags := range []string{
		"other/pkg=-X -X main.version=1.2.3",
		"-X other/pkg=-X",
		"-X main.version=1.2.3 -X main.version=",
		`"-X main.version=1.2.3"`,
		`-X"main.version=1.2.3"`,
		`-X main.version='1.2.3'`,
		"-X main.version=1.2.3\\ ",
		"-X main.version=1.2.3\u00a0",
		`"-X main.version=1.2.3`,
	} {
		if version, ok := embeddedVersionFromLDFlags(flags); ok {
			t.Errorf("embeddedVersionFromLDFlags(%q) = %q, true; want unknown", flags, version)
		}
	}
}

func buildVersionReaderFixture(t *testing.T, version string, release bool) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "version-reader.go")
	binary := filepath.Join(dir, "version-reader")
	program := `package main

import "fmt"

var version = "dev"

func main() { fmt.Print(version) }
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"build"}
	if release {
		args = append(args, "-ldflags", "-s -w -X main.version="+version)
	}
	args = append(args, "-o", binary, source)
	cmd := exec.CommandContext(ctx, "go", args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("build version reader timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("build version reader: %v\n%s", err, output)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	return binary
}
