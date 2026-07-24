package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestCmuxProbeUsesGlobalSystemTreeForExactSurface(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaces(testCmuxSurfaceID)}
	err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Probe(context.Background(), "cmux:surface:"+testCmuxSurfaceID)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != "/fake/cmux" || len(call.args) != 3 || call.args[0] != "rpc" || call.args[1] != "system.tree" || call.args[2] != "{}" {
		t.Fatalf("call = %#v, want raw global system.tree RPC", call)
	}
}

func TestCmuxProbeClassifiesSurfaceAbsentFromGlobalTree(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaces("B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2")}
	err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Probe(context.Background(), "cmux:surface:"+testCmuxSurfaceID)
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe() error = %v, want ErrTargetNotFound", err)
	}
}

func TestCmuxInventoryAnswersManyTargetsFromOneChildProcess(t *testing.T) {
	skipCmuxNonDarwin(t)
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaces(testCmuxSurfaceID, second)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	for _, target := range []string{"cmux:surface:" + testCmuxSurfaceID, "cmux:surface:" + second} {
		if err := inventory.Probe(target); err != nil {
			t.Fatalf("inventory Probe(%q) error = %v", target, err)
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want one system.tree inventory", len(runner.calls))
	}
}

func TestCmuxInventoryRejectsMultipleLiveAliasesForPhysicalTTY(t *testing.T) {
	skipCmuxNonDarwin(t)
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "/dev/ttys011"},
		map[string]string{"id": second, "tty": " ttys011 "},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	for _, target := range []string{"cmux:surface:" + testCmuxSurfaceID, "cmux:surface:" + second} {
		if _, err := inventory.OwnershipKey(target); err == nil || !strings.Contains(err.Error(), "2 live surface aliases") {
			t.Fatalf("OwnershipKey(%q) error = %v, want alias ambiguity", target, err)
		}
	}
}

func TestCmuxInventoryCanonicalizesBareTTYDeviceName(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": " ttys011 "},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	key, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "tty:/dev/ttys011" {
		t.Fatalf("OwnershipKey() = %q, %v, want canonical /dev tty", key, err)
	}
}

func TestCmuxInventoryRejectsBlankTTYInsteadOfAssumingUUIDOwnership(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "  "},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if err := inventory.Probe("cmux:surface:" + testCmuxSurfaceID); err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe() error = %v, want ambiguous missing-tty failure", err)
	}
}

func TestCmuxInventoryRejectsDuplicateSurfaceIDWithDifferentTTYs(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": strings.ToLower(testCmuxSurfaceID), "tty": "ttys012"},
	)}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Inventory(context.Background())
	if err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Inventory() error = %v, want ambiguous duplicate identity failure", err)
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

func TestCmuxInventoryRejectsMalformedTreeInsteadOfInferringAbsence(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaces("not-a-uuid")}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Inventory(context.Background())
	if err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Inventory() error = %v, want ambiguous parse failure", err)
	}
}

func TestCmuxInventoryRejectsMissingWindowsSchemaInsteadOfDetachingEverything(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte(`{"ok":true}`)}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Inventory(context.Background())
	if err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Inventory() error = %v, want ambiguous schema failure", err)
	}
}

func TestCmuxInjectUsesRawRPCThenSettlesAndSendsEnter(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaces(testCmuxSurfaceID)},
		{},
		{},
	}}
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
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %d, want inventory plus text and key", len(runner.calls))
	}
	if treeCall := runner.calls[0]; len(treeCall.args) < 2 || treeCall.args[1] != "system.tree" {
		t.Fatalf("first call = %#v, want target inventory", treeCall)
	}
	textCall := runner.calls[1]
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
	keyCall := runner.calls[2]
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
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaces(testCmuxSurfaceID)},
		{output: []byte("surface unavailable"), err: errors.New("exit status 1")},
	}}
	adapter := Cmux{Runner: runner, Path: "/fake/cmux"}
	err := adapter.Inject(context.Background(), "cmux:surface:"+testCmuxSurfaceID, "payload")
	if err == nil || !strings.Contains(err.Error(), "surface unavailable") {
		t.Fatalf("Inject() error = %v, want command output", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want inventory and failed text call only", len(runner.calls))
	}
}

func TestCmuxInjectRejectsDuplicateTTYAliasesBeforeSendingText(t *testing.T) {
	skipCmuxNonDarwin(t)
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": second, "tty": "/dev/ttys011"},
	)}
	err := (Cmux{Runner: runner, Path: "/fake/cmux"}).Inject(
		context.Background(), "cmux:surface:"+testCmuxSurfaceID, "payload",
	)
	if err == nil || !strings.Contains(err.Error(), "2 live surface aliases") {
		t.Fatalf("Inject() error = %v, want alias ambiguity", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].args[1] != "system.tree" {
		t.Fatalf("calls = %#v, want inventory only", runner.calls)
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

func cmuxTreeWithSurfaces(ids ...string) []byte {
	surfaces := make([]map[string]string, 0, len(ids))
	for index, id := range ids {
		surfaces = append(surfaces, map[string]string{"id": id, "tty": fmt.Sprintf("ttys%03d", index+1)})
	}
	return cmuxTreeWithSurfaceRecords(surfaces...)
}

func cmuxTreeWithSurfaceRecords(surfaces ...map[string]string) []byte {
	tree := map[string]any{
		"windows": []any{map[string]any{
			"workspaces": []any{map[string]any{
				"panes": []any{map[string]any{"surfaces": surfaces}},
			}},
		}},
	}
	data, err := json.Marshal(tree)
	if err != nil {
		panic(err)
	}
	return data
}
