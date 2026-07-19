package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testCmuxSurfaceID = "F901D722-6789-4BBB-9818-C4E97F20BEB3"

func TestCmuxDiscoverUsesExactSurfaceID(t *testing.T) {
	skipCmuxNonDarwin(t)
	adapter := Cmux{Getenv: func(key string) string {
		if key == "CMUX_SURFACE_ID" {
			return testCmuxSurfaceID
		}
		return ""
	}}
	target, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if want := "cmux:surface:" + testCmuxSurfaceID; target != want {
		t.Fatalf("target = %q, want %q", target, want)
	}
}

func TestCmuxDiscoverRejectsMissingSurfaceID(t *testing.T) {
	skipCmuxNonDarwin(t)
	_, err := (Cmux{Getenv: func(string) string { return "" }}).Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CMUX_SURFACE_ID") {
		t.Fatalf("Discover() error = %v, want CMUX_SURFACE_ID guidance", err)
	}
}

func TestParseCmuxSurfaceTargetRequiresUUID(t *testing.T) {
	id, err := parseCmuxSurfaceTarget(" cmux:surface:" + testCmuxSurfaceID + " ")
	if err != nil {
		t.Fatalf("parseCmuxSurfaceTarget() error = %v", err)
	}
	if id != testCmuxSurfaceID {
		t.Fatalf("id = %q, want %q", id, testCmuxSurfaceID)
	}
	for _, target := range []string{"cmux:surface:", "cmux:surface:surface:2", "ghostty:terminal:abc"} {
		if _, err := parseCmuxSurfaceTarget(target); err == nil {
			t.Fatalf("parseCmuxSurfaceTarget(%q) error = nil, want error", target)
		}
	}
}

func TestCmuxNormalizeTargetCanonicalizesUUIDCase(t *testing.T) {
	got, err := (Cmux{}).NormalizeTarget("cmux:surface:f901d722-6789-4bbb-9818-c4e97f20beb3")
	if err != nil {
		t.Fatalf("NormalizeTarget() error = %v", err)
	}
	if want := "cmux:surface:" + testCmuxSurfaceID; got != want {
		t.Fatalf("target = %q, want %q", got, want)
	}
}

func TestCmuxProbeUsesRawRPCForExactSurface(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte(`{"ok":true}`)}
	err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Probe(context.Background(), "cmux:surface:"+testCmuxSurfaceID)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != "/fake/cmux" || len(call.args) != 3 || call.args[0] != "rpc" || call.args[1] != "surface.read_text" {
		t.Fatalf("call = %#v, want raw surface.read_text RPC", call)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(call.args[2]), &params); err != nil {
		t.Fatalf("probe params are not JSON: %v", err)
	}
	if params["surface_id"] != testCmuxSurfaceID || params["lines"] != float64(1) {
		t.Fatalf("probe params = %#v, want exact surface and one line", params)
	}
}

func TestCmuxProbeClassifiesMissingWorkspace(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{
		output: []byte("Error: not_found: Workspace not found"),
		err:    errors.New("exit status 1"),
	}
	err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Probe(context.Background(), "cmux:surface:"+testCmuxSurfaceID)
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe() error = %v, want ErrTargetNotFound", err)
	}
}

func TestCmuxProbeDoesNotClassifyGenericFailureAsMissing(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte("cmux daemon unavailable"), err: errors.New("exit status 1")}
	err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Probe(context.Background(), "cmux:surface:"+testCmuxSurfaceID)
	if err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe() error = %v, want non-missing failure", err)
	}
}

func TestCmuxInjectUsesRawRPCThenSettlesAndSendsEnter(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{}
	var delays []time.Duration
	adapter := Cmux{
		Runner: runner,
		Path:   "/fake/cmux",
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}
	payload := `AMQ subject contains literal \n and --flags` + "\nsecond line\r\n"
	if err := adapter.Inject(context.Background(), "cmux:surface:"+testCmuxSurfaceID, payload); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(runner.calls))
	}
	textCall := runner.calls[0]
	if textCall.name != "/fake/cmux" || textCall.args[0] != "rpc" || textCall.args[1] != "surface.send_text" {
		t.Fatalf("text call = %#v, want raw surface.send_text RPC", textCall)
	}
	var textParams map[string]string
	if err := json.Unmarshal([]byte(textCall.args[2]), &textParams); err != nil {
		t.Fatalf("text params are not JSON: %v", err)
	}
	if got, want := textParams["text"], `AMQ subject contains literal \n and --flags`+"\nsecond line"; got != want {
		t.Fatalf("text = %q, want exact %q", got, want)
	}
	if textParams["surface_id"] != testCmuxSurfaceID {
		t.Fatalf("surface_id = %q, want %q", textParams["surface_id"], testCmuxSurfaceID)
	}
	if len(delays) != 1 || delays[0] != defaultCmuxSettleDelay {
		t.Fatalf("delays = %v, want [%s]", delays, defaultCmuxSettleDelay)
	}
	keyCall := runner.calls[1]
	if keyCall.args[0] != "rpc" || keyCall.args[1] != "surface.send_key" {
		t.Fatalf("key call = %#v, want raw surface.send_key RPC", keyCall)
	}
	var keyParams map[string]string
	if err := json.Unmarshal([]byte(keyCall.args[2]), &keyParams); err != nil {
		t.Fatalf("key params are not JSON: %v", err)
	}
	if keyParams["surface_id"] != testCmuxSurfaceID || keyParams["key"] != "enter" {
		t.Fatalf("key params = %#v, want exact surface enter", keyParams)
	}
}

func TestCmuxInjectDoesNotSendEnterWhenTextFails(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte("surface unavailable"), err: errors.New("exit status 1")}
	adapter := Cmux{Runner: runner, Path: "/fake/cmux"}
	err := adapter.Inject(context.Background(), "cmux:surface:"+testCmuxSurfaceID, "payload")
	if err == nil || !strings.Contains(err.Error(), "surface unavailable") {
		t.Fatalf("Inject() error = %v, want command output", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want failed text call only", len(runner.calls))
	}
}

func TestCmuxExecutablePrefersBundledEnvironmentPath(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "cmux")
	if err := os.WriteFile(bundled, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write bundled CLI: %v", err)
	}
	adapter := Cmux{
		Getenv: func(key string) string {
			if key == "CMUX_BUNDLED_CLI_PATH" {
				return bundled
			}
			return ""
		},
		LookPath: func(string) (string, error) {
			return "", errors.New("not found")
		},
	}
	got, err := adapter.executable()
	if err != nil {
		t.Fatalf("executable() error = %v", err)
	}
	if got != bundled {
		t.Fatalf("executable = %q, want bundled %q", got, bundled)
	}
}

func TestCmuxExecutableFallsBackToApplicationBundleWithoutPATH(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "Applications", "cmux.app", "Contents", "Resources", "bin", "cmux")
	if err := os.MkdirAll(filepath.Dir(bundled), 0o700); err != nil {
		t.Fatalf("mkdir bundled CLI: %v", err)
	}
	if err := os.WriteFile(bundled, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write bundled CLI: %v", err)
	}
	adapter := Cmux{
		Getenv: func(string) string { return "" },
		LookPath: func(string) (string, error) {
			return "", errors.New("not found")
		},
		UserHomeDir:  func() (string, error) { return dir, nil },
		IsExecutable: func(path string) bool { return path == bundled },
	}
	got, err := adapter.executable()
	if err != nil {
		t.Fatalf("executable() error = %v", err)
	}
	if got != bundled {
		t.Fatalf("executable = %q, want user bundle %q", got, bundled)
	}
}

func skipCmuxNonDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
}
