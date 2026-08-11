package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

const stderrDrainModeForTest = "AMQ_KEEPALIVE_INTERNAL_STDERR_DRAIN_V1=1"

func TestGetVersionPrefersInjectedVersion(t *testing.T) {
	previous := version
	version = "v1.2.3"
	t.Cleanup(func() { version = previous })

	if got := getVersion(); got != "v1.2.3" {
		t.Fatalf("getVersion() = %q, want v1.2.3", got)
	}
}

func TestResolveVersionFormatsBuildInfoFallback(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.modified", Value: "true"},
			{Key: "vcs.revision", Value: "abc123"},
		},
	}
	if got, want := resolveVersion("dev", info, true), "v1.2.3 vcs.revision=abc123 vcs.modified=true"; got != want {
		t.Fatalf("resolveVersion() = %q, want %q", got, want)
	}
}

func TestResolveVersionUsesVCSMetadataForPlainBuild(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
		},
	}
	if got, want := resolveVersion("dev", info, true), "dev vcs.revision=abc123"; got != want {
		t.Fatalf("resolveVersion() = %q, want %q", got, want)
	}
}

func TestResolveVersionFallsBackToDevWithoutBuildInfo(t *testing.T) {
	if got := resolveVersion("dev", nil, false); got != "dev" {
		t.Fatalf("resolveVersion() = %q, want dev", got)
	}
}

func TestIsVersionArgument(t *testing.T) {
	for _, args := range [][]string{{"-v"}, {"--version"}, {"version"}} {
		if !isVersionArgument(args) {
			t.Errorf("isVersionArgument(%q) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {"-V"}, {"version", "extra"}} {
		if isVersionArgument(args) {
			t.Errorf("isVersionArgument(%q) = true, want false", args)
		}
	}
}

func TestBuiltBinaryRunsStderrDrainBeforeArgumentHandling(t *testing.T) {
	binary := buildKeepaliveForTest(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	capture, err := os.CreateTemp(t.TempDir(), "stderr-capture-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capture.Close() }()

	cmd := exec.Command(binary, "__wake-stderr-drain")
	cmd.Env = append(os.Environ(), stderrDrainModeForTest)
	cmd.ExtraFiles = []*os.File{reader, capture}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if _, err := writer.WriteString("real-binary-drain-diagnostic\n"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("built drain exited with %v; stderr=%q", err, stderr.String())
	}

	got, err := os.ReadFile(capture.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "real-binary-drain-diagnostic\n" {
		t.Fatalf("capture = %q, want exact diagnostic", got)
	}
}

func TestBuiltBinaryReportsStderrDrainFailure(t *testing.T) {
	binary := buildKeepaliveForTest(t)
	// Construct the capture failure explicitly: fd 3 delivers real bytes and
	// fd 4 is read-only, so the drain's capture write must fail. Relying on
	// fd 3/4 being closed is not hermetic — an ancestor (git's pre-push hook
	// chain, make) can leak a live descriptor without close-on-exec, and the
	// drain then blocks reading it until the package times out.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	readOnlyCapture, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readOnlyCapture.Close() }()

	cmd := exec.Command(binary, "__wake-stderr-drain")
	cmd.Env = append(os.Environ(), stderrDrainModeForTest)
	cmd.ExtraFiles = []*os.File{reader, readOnlyCapture}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("diagnostic that cannot be captured\n"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if err := cmd.Wait(); err == nil {
		t.Fatalf("built drain unexpectedly succeeded; output=%q", output.String())
	}
	if !strings.Contains(output.String(), "amq-keepalive stderr drain failed: capture wake stderr") {
		t.Fatalf("output = %q, want concrete drain failure", output.String())
	}
}

func TestBuiltBinaryDoesNotEnterStderrDrainFromEnvironmentAlone(t *testing.T) {
	binary := buildKeepaliveForTest(t)
	cmd := exec.Command(binary, "not-a-valid-user-command")
	cmd.Env = append(os.Environ(), stderrDrainModeForTest)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("unknown command unexpectedly succeeded; output=%q", output)
	}
	if strings.Contains(string(output), "amq-keepalive stderr drain failed") {
		t.Fatalf("environment alone activated private stderr drain: %q", output)
	}
}

func buildKeepaliveForTest(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "amq-keepalive")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build amq-keepalive: %v\n%s", err, output)
	}
	return binary
}
