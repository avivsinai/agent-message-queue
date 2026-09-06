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

func TestCmuxProbeUsesGlobalSystemTreeForExactSurface(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaces(testCmuxSurfaceID)}
	err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Probe(context.Background(), "cmux:surface:"+testCmuxSurfaceID)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != "/fake/cmux" || len(call.args) != 3 || call.args[0] != "rpc" || call.args[1] != "system.tree" || call.args[2] != cmuxSystemTreeParams {
		t.Fatalf("call = %#v, want all-windows system.tree RPC", call)
	}
}

func TestCmuxInventoryIncludesSurfaceInNonFocusedWindow(t *testing.T) {
	skipCmuxNonDarwin(t)
	nonFocused := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithWindows(
		map[string]any{
			"is_focused": true,
			"workspaces": []any{map[string]any{
				"id":    testCmuxWorkspaceID,
				"panes": []any{map[string]any{"surfaces": []any{}}},
			}},
		},
		map[string]any{
			"is_focused": false,
			"workspaces": []any{map[string]any{
				"id":    testCmuxWorkspaceID,
				"panes": []any{map[string]any{"surfaces": []any{map[string]any{"id": nonFocused, "tty": "ttys002"}}}},
			}},
		},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if err := inventory.Probe("cmux:surface:" + nonFocused); err != nil {
		t.Fatalf("Probe(non-focused surface) error = %v", err)
	}
	if got := runner.calls[0].args[2]; got != cmuxSystemTreeParams {
		t.Fatalf("system.tree params = %q, want %q; empty request excludes non-focused windows", got, cmuxSystemTreeParams)
	}
}

func TestCmuxInventoryAnswersManyTargetsFromOneChildProcess(t *testing.T) {
	skipCmuxNonDarwin(t)
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaces(testCmuxSurfaceID, second)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
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
	// Fail-closed variant: no trusted candidate (supervisor/inject pass) and the
	// kernel proves at least one live owner, but two surfaces claim the tty. We
	// cannot tell which is live, so ownership stays ambiguous and nothing is
	// evicted.
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "/dev/ttys011"},
		map[string]string{"id": second, "tty": " ttys011 "},
	)}
	liveness := &fakeTTYLiveness{count: 1}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveness.ownerCount}).
		Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	for _, target := range []string{"cmux:surface:" + testCmuxSurfaceID, "cmux:surface:" + second} {
		_, err := inventory.OwnershipKey(target)
		if err == nil || !strings.Contains(err.Error(), "2 live surface aliases") {
			t.Fatalf("OwnershipKey(%q) error = %v, want alias ambiguity", target, err)
		}
		key, ok := CmuxDegradedOwnershipKey(inventory, err)
		if !ok || key != "tty:/dev/ttys011" {
			t.Fatalf("CmuxDegradedOwnershipKey(%q) = %q, %v; want canonical tty key", target, key, ok)
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want one system.tree and no eviction", len(runner.calls))
	}
}

func TestCmuxOwnershipKeyRejectsTTYChangeAfterRecordedTTY(t *testing.T) {
	skipCmuxNonDarwin(t)
	recorded := newCmuxOwnershipRecord()
	first := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
	)}
	firstInv, err := (Cmux{Runner: first, Path: "/fake/cmux", recorded: recorded, LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("first Inventory() error = %v", err)
	}
	key, err := firstInv.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "tty:/dev/ttys011" {
		t.Fatalf("first OwnershipKey() = %q, %v", key, err)
	}
	second := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys012"},
	)}
	secondInv, err := (Cmux{Runner: second, Path: "/fake/cmux", recorded: recorded, LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("second Inventory() error = %v", err)
	}
	_, err = secondInv.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err == nil || !errors.Is(err, ErrTargetDegraded) || !strings.Contains(err.Error(), "ownership key conflict") {
		t.Fatalf("second OwnershipKey() error = %v, want recorded tty conflict", err)
	}
}

func TestCanonicalCmuxTTYRejectsAliasesAndNonPTYValues(t *testing.T) {
	for _, value := range []string{
		"../dev/ttys011",
		"nested/ttys011",
		"/tmp/../dev/ttys011",
		"/dev//ttys011",
		"/private/dev/ttys011",
		"/dev/ttys",
		"/dev/ttys-1",
		"/dev/ttys01x",
		"/dev/ptys011",
		"123",
		"/dev/123",
	} {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			if got, err := canonicalCmuxTTY(value); err == nil || got != "" {
				t.Fatalf("canonicalCmuxTTY(%q) = %q, %v; want rejection", value, got, err)
			}
		})
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
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: liveOwner,
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

func TestCmuxInjectEnterFailureAfterTextIsUncertain(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaces(testCmuxSurfaceID)},
		{},
		{output: []byte("enter failed"), err: errors.New("exit status 1")},
	}}
	adapter := Cmux{
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: liveOwner,
		Sleep:             func(context.Context, time.Duration) error { return nil },
	}
	err := adapter.Inject(context.Background(), "cmux:surface:"+testCmuxSurfaceID, "payload")
	if !errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("Inject() error = %v, want ErrInjectUncertain", err)
	}
	if !strings.Contains(err.Error(), "enter failed") {
		t.Fatalf("Inject() error = %v, want command output", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %d, want inventory, text, and failed enter", len(runner.calls))
	}
	if got := runner.calls[1].args[1]; got != "surface.send_text" {
		t.Fatalf("text call = %#v, want surface.send_text", runner.calls[1])
	}
	if got := runner.calls[2].args[1]; got != "surface.send_key" {
		t.Fatalf("key call = %#v, want surface.send_key", runner.calls[2])
	}
}

func TestCmuxInjectEvictsProcessDeadAliasThenSendsText(t *testing.T) {
	skipCmuxNonDarwin(t)
	// A no-trust pass may still evict a surface cmux reports process_alive:false.
	// Once the corpse alias is retracted, the live target owns the tty and text
	// injection proceeds.
	corpse := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
			map[string]string{"id": corpse, "tty": "ttys011", "process_alive": "false"},
		)},
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
			map[string]string{"id": corpse, "tty": "ttys011", "process_alive": "false"},
		)}, // CAS snapshot: corpse still stale
		{}, // surface.report_tty
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
			map[string]string{"id": corpse, "tty": cmuxEvictedTTYName},
		)},
		{}, // surface.send_text
		{}, // surface.send_key
	}}
	liveness := &fakeTTYLiveness{count: 1}
	adapter := Cmux{
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: liveness.ownerCount,
		Sleep:             func(context.Context, time.Duration) error { return nil },
	}
	if err := adapter.Inject(context.Background(), "cmux:surface:"+testCmuxSurfaceID, "payload"); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	wantSequence := []string{"system.tree", "system.tree", "surface.report_tty", "system.tree", "surface.send_text", "surface.send_key"}
	if len(runner.calls) != len(wantSequence) {
		t.Fatalf("calls = %d, want %d (%v)", len(runner.calls), len(wantSequence), wantSequence)
	}
	for i, want := range wantSequence {
		if runner.calls[i].args[1] != want {
			t.Fatalf("call[%d] = %q, want %q", i, runner.calls[i].args[1], want)
		}
	}
	var evictParams map[string]string
	if err := json.Unmarshal([]byte(runner.calls[2].args[2]), &evictParams); err != nil {
		t.Fatalf("report_tty params are not JSON: %v", err)
	}
	if evictParams["surface_id"] != corpse || evictParams["tty_name"] != cmuxEvictedTTYName {
		t.Fatalf("report_tty params = %#v, want corpse retracted to sentinel", evictParams)
	}
}

func TestCmuxInventoryDoesNotOverwriteLiveRebindBeforeEvict(t *testing.T) {
	skipCmuxNonDarwin(t)
	// Queued stale eviction must not report_tty after the surface rebinds to a
	// different live PTY. There is no CAS RPC, so the confirmation snapshot is
	// the compare: mismatch aborts and degrades.
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		)},
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys012"},
		)},
	}}
	inventory, err := (Cmux{
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: staleOwner,
	}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID); !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("OwnershipKey() error = %v, want degraded after live rebind", err)
	}
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[1] == "surface.report_tty" {
			t.Fatalf("report_tty overwrote a live rebind: %#v", call)
		}
	}
}

func TestCmuxExecutablePrefersBundledEnvironmentPath(t *testing.T) {
	skipCmuxNonDarwin(t)
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

func skipCmuxNonDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("cmux adapter requires macOS")
	}
}

func liveOwner(string) (int, error) { return 1, nil }

func staleOwner(string) (int, error) { return 0, nil }

const testCmuxWorkspaceID = "WS-TEST-0001"

func cmuxTreeWithSurfaces(ids ...string) []byte {
	surfaces := make([]map[string]string, 0, len(ids))
	for index, id := range ids {
		surfaces = append(surfaces, map[string]string{"id": id, "tty": fmt.Sprintf("ttys%03d", index+1)})
	}
	return cmuxTreeWithSurfaceRecords(surfaces...)
}

func cmuxTreeWithSurfaceRecords(surfaces ...map[string]string) []byte {
	return cmuxTreeWithWorkspace(testCmuxWorkspaceID, surfaces...)
}

// cmuxTreeWithWorkspace emits a single-workspace tree with the given workspace
// id. Surface records are string maps for backward compatibility; the special
// "process_alive" key (value "true"/"false") is emitted as a JSON boolean so it
// unmarshals into the *bool field. Omit the key for the absent/unknown case.
func cmuxTreeWithWorkspace(workspaceID string, surfaces ...map[string]string) []byte {
	records := make([]map[string]any, 0, len(surfaces))
	for _, surface := range surfaces {
		record := map[string]any{}
		for key, value := range surface {
			if key == "process_alive" {
				record[key] = value == "true"
				continue
			}
			record[key] = value
		}
		if _, ok := record["type"]; !ok {
			record["type"] = "terminal"
		}
		records = append(records, record)
	}
	return cmuxTreeWithWindows(map[string]any{
		"workspaces": []any{map[string]any{
			"id":    workspaceID,
			"panes": []any{map[string]any{"surfaces": records}},
		}},
	})
}

func cmuxTreeWithWindows(windows ...map[string]any) []byte {
	tree := map[string]any{"windows": windows}
	data, err := json.Marshal(tree)
	if err != nil {
		panic(err)
	}
	return data
}

// fakeTTYLiveness is an injectable LiveTTYOwnerCount seam. Cmux tests always
// inject it so they never run a real sysctl against fixture tty names.
type fakeTTYLiveness struct {
	count int
	err   error
	calls []string
}

func (f *fakeTTYLiveness) ownerCount(devPath string) (int, error) {
	f.calls = append(f.calls, devPath)
	return f.count, f.err
}

// Restored from origin/main during the round-2 cull: the only proof that
// recorded ownership survives a fresh adapter and refuses a conflicting tty
// (ownership-key drift). Covers WithOwnershipRecord, RememberOwnership,
// sameCmuxSurfaceIdentity, and the fail-closed ownership-degradation contract.
func TestCmuxRememberOwnershipRefusesTTYDriftOnFreshAdapter(t *testing.T) {
	skipCmuxNonDarwin(t)
	first := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
	)}
	firstCmux := Cmux{Runner: first, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}.WithOwnershipRecord()
	firstInv, err := firstCmux.Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("first Inventory() error = %v", err)
	}
	key, err := firstInv.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "tty:/dev/ttys011" {
		t.Fatalf("first OwnershipKey() = %q, %v", key, err)
	}

	second := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys012"},
	)}
	secondCmux := Cmux{Runner: second, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}.WithOwnershipRecord()
	secondCmux.RememberOwnership("cmux:surface:"+testCmuxSurfaceID, key)
	secondInv, err := secondCmux.Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("second Inventory() error = %v", err)
	}
	_, err = secondInv.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err == nil || !errors.Is(err, ErrTargetDegraded) || !strings.Contains(err.Error(), "ownership key conflict") {
		t.Fatalf("second OwnershipKey() error = %v, want recorded tty conflict", err)
	}
}

// Restored from origin/main during the round-2 cull: the direct happy-path proof
// that Cmux.NormalizeTarget canonicalizes a cmux:surface:<uuid> target. Covers
// the target-normalization user behavior.
func TestCmuxNormalizeTargetCanonicalizesUUIDCase(t *testing.T) {
	got, err := (Cmux{}).NormalizeTarget("cmux:surface:f901d722-6789-4bbb-9818-c4e97f20beb3")
	if err != nil {
		t.Fatalf("NormalizeTarget() error = %v", err)
	}
	if want := "cmux:surface:" + testCmuxSurfaceID; got != want {
		t.Fatalf("target = %q, want %q", got, want)
	}
}
