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

type foreignTargetInventory struct{}

func (foreignTargetInventory) Probe(string) error { return nil }

func (foreignTargetInventory) OwnershipKey(string) (string, error) { return "foreign:key", nil }

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

func TestCmuxProbeClassifiesSurfaceAbsentFromGlobalTree(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaces("B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2")}
	err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Probe(context.Background(), "cmux:surface:"+testCmuxSurfaceID)
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe() error = %v, want ErrTargetNotFound", err)
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

func TestCmuxInventoryKeepsTrustedCandidateAmbiguousWithoutDeadProof(t *testing.T) {
	skipCmuxNonDarwin(t)
	// CMUX_SURFACE_ID proves the candidate exists, not that every co-claimant is
	// dead. Preserve both aliases until cmux explicitly marks one dead or the
	// kernel proves there are no live tty owners.
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": second, "tty": "/dev/ttys011"},
	)}
	liveness := &fakeTTYLiveness{count: 1}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveness.ownerCount}).
		Inventory(context.Background(), trustedCmux(testCmuxSurfaceID))
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	for _, target := range []string{testCmuxSurfaceID, second} {
		if _, err := inventory.OwnershipKey("cmux:surface:" + target); err == nil || !strings.Contains(err.Error(), "2 live surface aliases") {
			t.Fatalf("OwnershipKey(%q) error = %v, want alias ambiguity", target, err)
		}
	}
	if len(liveness.calls) != 1 {
		t.Fatalf("liveness calls = %v, want one tty ownership probe", liveness.calls)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want one system.tree and no eviction", len(runner.calls))
	}
}

// assertCmuxEviction verifies exactly one surface.report_tty eviction of the
// given surface plus inventory, CAS snapshot, and rebuild system.tree calls.
// The extra tree is a CAS snapshot: report_tty is issued only when the corpse
// still claims the same tty, so a live rebind cannot be overwritten.
func assertCmuxEviction(t *testing.T, runner *fakeCommandRunner, corpseID string) {
	t.Helper()
	trees := 0
	var evict *commandCall
	for i := range runner.calls {
		call := &runner.calls[i]
		if len(call.args) < 2 {
			continue
		}
		switch call.args[1] {
		case "system.tree":
			trees++
		case "surface.report_tty":
			if evict != nil {
				t.Fatalf("multiple report_tty calls: %#v", runner.calls)
			}
			evict = call
		}
	}
	if trees != 3 {
		t.Fatalf("system.tree calls = %d, want inventory + cas + rebuild; calls=%#v", trees, runner.calls)
	}
	if evict == nil {
		t.Fatalf("missing surface.report_tty; calls=%#v", runner.calls)
	}
	if len(evict.args) != 3 || evict.args[0] != "rpc" {
		t.Fatalf("evict call = %#v, want surface.report_tty RPC", *evict)
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(evict.args[2]), &params); err != nil {
		t.Fatalf("report_tty params are not JSON: %v", err)
	}
	if params["surface_id"] != corpseID {
		t.Fatalf("evicted surface_id = %q, want corpse %q", params["surface_id"], corpseID)
	}
	if params["workspace_id"] != testCmuxWorkspaceID {
		t.Fatalf("evicted workspace_id = %q, want %q", params["workspace_id"], testCmuxWorkspaceID)
	}
	if params["tty_name"] != cmuxEvictedTTYName {
		t.Fatalf("evicted tty_name = %q, want sentinel %q", params["tty_name"], cmuxEvictedTTYName)
	}
}

func TestCmuxInventoryCanonicalizesBareTTYDeviceName(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": " ttys011 "},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	key, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "tty:/dev/ttys011" {
		t.Fatalf("OwnershipKey() = %q, %v, want canonical /dev tty", key, err)
	}
}

func TestCmuxInventoryUsesSurfaceUUIDWhenTTYBlank(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "  "},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	key, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "surface:"+testCmuxSurfaceID {
		t.Fatalf("OwnershipKey() = %q, %v, want surface UUID key", key, err)
	}
}

func TestCmuxInventoryRejectsNonTerminalType(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "type": "browser", "tty": "ttys011"},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	err = inventory.Probe("cmux:surface:" + testCmuxSurfaceID)
	if err == nil || errors.Is(err, ErrTargetNotFound) || !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("Probe() error = %v, want degraded non-terminal type", err)
	}
	if key, ok := CmuxDegradedOwnershipKey(inventory, err); ok || key != "" {
		t.Fatalf("CmuxDegradedOwnershipKey(non-terminal) = %q, %v; want unavailable", key, ok)
	}
}

func TestCmuxInventoryAcceptsMixedCaseTerminalType(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "type": "Terminal", "tty": "ttys011"},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	key, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "tty:/dev/ttys011" {
		t.Fatalf("OwnershipKey() = %q, %v, want tty for mixed-case Terminal type", key, err)
	}
}

func TestCmuxOwnershipKeyPromotesBlankTTYToCanonicalTTYOnce(t *testing.T) {
	skipCmuxNonDarwin(t)
	recorded := newCmuxOwnershipRecord()
	blank := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": ""},
	)}
	blankInv, err := (Cmux{Runner: blank, Path: "/fake/cmux", recorded: recorded, LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("blank Inventory() error = %v", err)
	}
	key, err := blankInv.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "surface:"+testCmuxSurfaceID {
		t.Fatalf("blank OwnershipKey() = %q, %v, want surface UUID", key, err)
	}
	present := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
	)}
	presentInv, err := (Cmux{Runner: present, Path: "/fake/cmux", recorded: recorded, LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("present Inventory() error = %v", err)
	}
	key, err = presentInv.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "tty:/dev/ttys011" {
		t.Fatalf("promoted OwnershipKey() = %q, %v, want tty", key, err)
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

func TestCmuxOwnershipKeyAcceptsRepeatedIdenticalTTY(t *testing.T) {
	skipCmuxNonDarwin(t)
	recorded := newCmuxOwnershipRecord()
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
	)}
	firstInv, err := (Cmux{Runner: runner, Path: "/fake/cmux", recorded: recorded, LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("first Inventory() error = %v", err)
	}
	first, err := firstInv.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || first != "tty:/dev/ttys011" {
		t.Fatalf("first OwnershipKey() = %q, %v", first, err)
	}
	secondInv, err := (Cmux{Runner: runner, Path: "/fake/cmux", recorded: recorded, LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("second Inventory() error = %v", err)
	}
	second, err := secondInv.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || second != first {
		t.Fatalf("repeated identical OwnershipKey() = %q, %v; want %q", second, err, first)
	}
}

func TestCmuxInventoryRejectsDuplicateSurfaceIDWhenProcessAliveOmitted(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte(`{
		"windows":[{
			"workspaces":[{"id":"WS-1","panes":[{"surfaces":[
				{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"ttys011"},
				{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"ttys011","process_alive":true}
			]}]}]
		}]
	}`)}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err == nil || !strings.Contains(err.Error(), "conflicting tty identities") {
		t.Fatalf("Inventory() error = %v, want nil vs set process_alive conflict", err)
	}
}

func TestCmuxInventoryRejectsNonPTYPathWithoutOwnershipKey(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "/dev/not-a-tty"},
	)}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	err = inventory.Probe("cmux:surface:" + testCmuxSurfaceID)
	if err == nil || !errors.Is(err, ErrTargetDegraded) || !strings.Contains(err.Error(), "not a macOS PTY") {
		t.Fatalf("Probe() error = %v, want keyless non-PTY degradation", err)
	}
	if key, ok := CmuxDegradedOwnershipKey(inventory, err); ok || key != "" {
		t.Fatalf("CmuxDegradedOwnershipKey(non-PTY) = %q, %v; want unavailable", key, ok)
	}
}

func TestCanonicalCmuxTTYAcceptsOnlyDocumentedForms(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "basename", value: "ttys011", want: "/dev/ttys011"},
		{name: "trimmed basename", value: " ttys012 ", want: "/dev/ttys012"},
		{name: "absolute", value: "/dev/ttys013", want: "/dev/ttys013"},
		{name: "eviction basename", value: cmuxEvictedTTYName, want: cmuxEvictedTTY},
		{name: "eviction absolute", value: cmuxEvictedTTY, want: cmuxEvictedTTY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := canonicalCmuxTTY(tt.value)
			if err != nil || got != tt.want {
				t.Fatalf("canonicalCmuxTTY(%q) = %q, %v; want %q, nil", tt.value, got, err, tt.want)
			}
		})
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

func TestCmuxDegradedOwnershipKeyRejectsForeignInventory(t *testing.T) {
	first := newCmuxInventory(nil, cmuxResolution{})
	second := newCmuxInventory(nil, cmuxResolution{})
	err := &cmuxDegradedOwnershipError{
		inventoryToken: first.ownershipToken,
		ownershipKey:   "tty:/dev/ttys011",
		detail:         "ambiguous",
	}
	if key, ok := CmuxDegradedOwnershipKey(foreignTargetInventory{}, err); ok || key != "" {
		t.Fatalf("CmuxDegradedOwnershipKey(foreign inventory) = %q, %v; want unavailable", key, ok)
	}
	if key, ok := CmuxDegradedOwnershipKey(second, err); ok || key != "" {
		t.Fatalf("CmuxDegradedOwnershipKey(other cmux snapshot) = %q, %v; want unavailable", key, ok)
	}
	if key, ok := CmuxDegradedOwnershipKey(first, fmt.Errorf("wrapped: %w", ErrTargetDegraded)); ok || key != "" {
		t.Fatalf("CmuxDegradedOwnershipKey(generic error) = %q, %v; want unavailable", key, ok)
	}
}

func TestCmuxInventoryRejectsDuplicateSurfaceIDWithDifferentTTYs(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": strings.ToLower(testCmuxSurfaceID), "tty": "ttys012"},
	)}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Inventory() error = %v, want ambiguous duplicate identity failure", err)
	}
}

func TestCmuxInventoryRejectsDuplicateSurfaceIDWithDifferentWorkspace(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte(`{
		"windows":[{
			"workspaces":[
				{"id":"WS-1","panes":[{"surfaces":[{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"ttys011"}]}]},
				{"id":"WS-2","panes":[{"surfaces":[{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"ttys011"}]}]}
			]
		}]
	}`)}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err == nil || !strings.Contains(err.Error(), "conflicting tty identities") {
		t.Fatalf("Inventory() error = %v, want conflicting workspace identity failure", err)
	}
}

func TestCmuxInventoryRejectsDuplicateSurfaceIDWithDifferentProcessAlive(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte(`{
		"windows":[{
			"workspaces":[{"id":"WS-1","panes":[{"surfaces":[
				{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"ttys011","process_alive":true},
				{"id":"F901D722-6789-4BBB-9818-C4E97F20BEB3","tty":"ttys011","process_alive":false}
			]}]}]
		}]
	}`)}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err == nil || !strings.Contains(err.Error(), "conflicting tty identities") {
		t.Fatalf("Inventory() error = %v, want conflicting process liveness failure", err)
	}
}

func TestCmuxProbeDoesNotClassifyGenericFailureAsMissing(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte("cmux daemon unavailable"), err: errors.New("exit status 1")}
	err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Probe(context.Background(), "cmux:surface:"+testCmuxSurfaceID)
	if err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe() error = %v, want non-missing failure", err)
	}
}

func TestCmuxInventoryRejectsMalformedTreeInsteadOfInferringAbsence(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaces("not-a-uuid")}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
	if err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Inventory() error = %v, want ambiguous parse failure", err)
	}
}

func TestCmuxInventoryRejectsMissingWindowsSchemaInsteadOfDetachingEverything(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte(`{"ok":true}`)}
	_, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}).Inventory(context.Background(), OwnershipContext{})
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

func TestCmuxInjectDoesNotSendEnterWhenTextFails(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaces(testCmuxSurfaceID)},
		{output: []byte("surface unavailable"), err: errors.New("exit status 1")},
	}}
	adapter := Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveOwner}
	err := adapter.Inject(context.Background(), "cmux:surface:"+testCmuxSurfaceID, "payload")
	if err == nil || !strings.Contains(err.Error(), "surface unavailable") {
		t.Fatalf("Inject() error = %v, want command output", err)
	}
	if errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("text failure marked uncertain: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want inventory and failed text call only", len(runner.calls))
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
		Runner: runner,
		Path:   "/fake/cmux",
		Sleep:  func(context.Context, time.Duration) error { return nil },
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

func TestCmuxInjectRejectsDuplicateTTYAliasesBeforeSendingText(t *testing.T) {
	skipCmuxNonDarwin(t)
	// Fail-closed variant: Inject passes no trusted candidate, so two live
	// claimants on one tty with no corpse signal stay ambiguous and no text is
	// sent.
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": second, "tty": "/dev/ttys011"},
	)}
	liveness := &fakeTTYLiveness{count: 1}
	err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveness.ownerCount}).Inject(
		context.Background(), "cmux:surface:"+testCmuxSurfaceID, "payload",
	)
	if err == nil || !strings.Contains(err.Error(), "2 live surface aliases") {
		t.Fatalf("Inject() error = %v, want alias ambiguity", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].args[1] != "system.tree" {
		t.Fatalf("calls = %#v, want inventory only", runner.calls)
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

func TestCmuxEvictDoesNotReportTTYWhenWorkspaceIDEmpty(t *testing.T) {
	skipCmuxNonDarwin(t)
	corpse := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithWorkspace("",
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": corpse, "tty": "ttys011", "process_alive": "false"},
	)}
	liveness := &fakeTTYLiveness{count: 1}
	err := (Cmux{
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: liveness.ownerCount,
		Sleep:             func(context.Context, time.Duration) error { return nil },
	}).Inject(context.Background(), "cmux:surface:"+testCmuxSurfaceID, "payload")
	if err == nil {
		t.Fatal("Inject() succeeded; empty workspace id must fail closed without report_tty")
	}
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[1] == "surface.report_tty" {
			t.Fatalf("report_tty with empty workspace id: %#v", call)
		}
	}
	trees := 0
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[1] == "system.tree" {
			trees++
		}
	}
	if trees != 2 {
		t.Fatalf("calls = %#v, want inventory + cas snapshot and no report_tty", runner.calls)
	}
}

func TestCmuxInventoryFailsClosedWhenTrustedCandidateAbsentEvenIfOneClaimantRegistered(t *testing.T) {
	skipCmuxNonDarwin(t)
	// Inverse of the trusted-candidate resolution: with no trusted candidate a
	// registered entry may itself be the corpse, so "registered wins" is
	// forbidden. Two claimants + one live owner + no trust -> fail closed, and
	// crucially ZERO evictions (we must not retract a possibly-live alias).
	registeredCorpse := testCmuxSurfaceID
	unregisteredLive := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": registeredCorpse, "tty": "ttys011"},
		map[string]string{"id": unregisteredLive, "tty": "ttys011"},
	)}
	liveness := &fakeTTYLiveness{count: 1}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveness.ownerCount}).
		Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + registeredCorpse); err == nil || !strings.Contains(err.Error(), "2 live surface aliases") {
		t.Fatalf("OwnershipKey(registered) error = %v, want ambiguity", err)
	}
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[1] == "surface.report_tty" {
			t.Fatalf("unexpected eviction with no trusted candidate: %#v", call)
		}
	}
}

func TestCmuxInventoryEvictsAllClaimantsWhenTTYHasNoLiveOwner(t *testing.T) {
	skipCmuxNonDarwin(t)
	// Zero live kernel owners means every claimant is a corpse: evict them all
	// and classify the targets absent.
	a := testCmuxSurfaceID
	b := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": a, "tty": "ttys011"},
			map[string]string{"id": b, "tty": "ttys011"},
		)},
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": a, "tty": "ttys011"},
			map[string]string{"id": b, "tty": "ttys011"},
		)}, // CAS: both claimants still stale
		{}, // report_tty for first corpse
		{}, // report_tty for second corpse
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": a, "tty": cmuxEvictedTTYName},
			map[string]string{"id": b, "tty": cmuxEvictedTTYName},
		)},
	}}
	liveness := &fakeTTYLiveness{count: 0}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveness.ownerCount}).
		Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	for _, id := range []string{a, b} {
		if _, err := inventory.OwnershipKey("cmux:surface:" + id); !errors.Is(err, ErrTargetNotFound) {
			t.Fatalf("OwnershipKey(%q) error = %v, want ErrTargetNotFound", id, err)
		}
	}
	evictions := 0
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[1] == "surface.report_tty" {
			evictions++
		}
	}
	if evictions != 2 {
		t.Fatalf("report_tty calls = %d, want 2 (both corpses evicted)", evictions)
	}
}

func TestCmuxInventoryEvictsProcessDeadAliasAndKeepsLiveOwner(t *testing.T) {
	skipCmuxNonDarwin(t)
	corpse := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
			map[string]string{"id": corpse, "tty": "ttys011", "process_alive": "false"},
		)},
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
			map[string]string{"id": corpse, "tty": "ttys011", "process_alive": "false"},
		)}, // CAS: corpse still process_alive:false
		{}, // report_tty
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
			map[string]string{"id": corpse, "tty": cmuxEvictedTTYName},
		)},
	}}
	liveness := &fakeTTYLiveness{count: 1}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveness.ownerCount}).
		Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	key, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "tty:/dev/ttys011" {
		t.Fatalf("OwnershipKey(live) = %q, %v, want owned tty", key, err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + corpse); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("OwnershipKey(process-dead) error = %v, want ErrTargetNotFound", err)
	}
	assertCmuxEviction(t, runner, corpse)
}

func TestCmuxInventoryTreatsAbsentProcessAliveAsUnknown(t *testing.T) {
	skipCmuxNonDarwin(t)
	// process_alive absent must not mark a surface a corpse. Two live claimants
	// with no liveness field and no trusted candidate stay ambiguous — nothing
	// is treated as dead and nothing is evicted.
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": second, "tty": "ttys011"},
	)}
	liveness := &fakeTTYLiveness{count: 2}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveness.ownerCount}).
		Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID); err == nil || !strings.Contains(err.Error(), "2 live surface aliases") {
		t.Fatalf("OwnershipKey() error = %v, want ambiguity", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want one system.tree and no eviction", len(runner.calls))
	}
}

func TestCmuxInventoryIgnoresSentinelTTYSurface(t *testing.T) {
	skipCmuxNonDarwin(t)
	// A surface already carrying the eviction sentinel is retired: it is a
	// claimant of no tty and resolves to absent, so a co-listed live surface
	// owns its tty cleanly without contention.
	retired := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": retired, "tty": cmuxEvictedTTYName},
	)}
	liveness := &fakeTTYLiveness{count: 1}
	inventory, err := (Cmux{Runner: runner, Path: "/fake/cmux", LiveTTYOwnerCount: liveness.ownerCount}).
		Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	key, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID)
	if err != nil || key != "tty:/dev/ttys011" {
		t.Fatalf("OwnershipKey(live) = %q, %v, want owned tty", key, err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + retired); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("OwnershipKey(retired) error = %v, want ErrTargetNotFound", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want one system.tree and no eviction", len(runner.calls))
	}
}

func TestCmuxInventoryPreservesAmbiguityWhenEvictionFails(t *testing.T) {
	skipCmuxNonDarwin(t)
	// report_tty fails: the corpse is not retracted, so the tty stays ambiguous
	// (fail-closed), a WARN is logged, and the pass neither rebuilds in a loop
	// nor crashes.
	corpse := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
			map[string]string{"id": corpse, "tty": "ttys011", "process_alive": "false"},
		)},
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
			map[string]string{"id": corpse, "tty": "ttys011", "process_alive": "false"},
		)}, // CAS
		{output: []byte("surface gone"), err: errors.New("exit status 1")},
	}}
	liveness := &fakeTTYLiveness{count: 1}
	var logs []string
	inventory, err := (Cmux{
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: liveness.ownerCount,
		Logf:              captureLogf(&logs),
	}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID); !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("OwnershipKey() error = %v, want degraded ownership after failed eviction", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %d, want tree + cas + failed report_tty only (no rebuild loop)", len(runner.calls))
	}
	if !containsSubstring(logs, "cmux eviction failed") {
		t.Fatalf("logs = %v, want eviction-failure WARN", logs)
	}
}

func TestCmuxInventoryPartialEvictionFailureStaysFailClosedAndRetries(t *testing.T) {
	skipCmuxNonDarwin(t)
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	third := "C9B9D5B8-4D99-4EAE-A4CF-A8F0812E18E3"
	initial := cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": second, "tty": "ttys011"},
		map[string]string{"id": third, "tty": "ttys011"},
	)
	rebuilt := cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": cmuxEvictedTTYName},
		map[string]string{"id": second, "tty": cmuxEvictedTTYName},
		map[string]string{"id": third, "tty": "ttys011"},
	)
	onePass := []fakeCommandResult{
		{output: initial},
		{output: initial}, // CAS: remaining corpses still stale
		{},
		{},
		{output: []byte("surface busy"), err: errors.New("exit status 1")},
		{output: rebuilt},
	}
	runner := &fakeCommandRunner{results: append(append([]fakeCommandResult{}, onePass...), onePass...)}
	liveness := &fakeTTYLiveness{count: 0}
	var logs []string
	cmux := Cmux{
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: liveness.ownerCount,
		Logf:              captureLogf(&logs),
	}
	for pass := 1; pass <= 2; pass++ {
		inventory, err := cmux.Inventory(context.Background(), OwnershipContext{})
		if err != nil {
			t.Fatalf("pass %d Inventory() error = %v", pass, err)
		}
		if _, err := inventory.OwnershipKey("cmux:surface:" + third); err == nil || errors.Is(err, ErrTargetNotFound) {
			t.Fatalf("pass %d failed corpse error = %v, want fail-closed ambiguity", pass, err)
		}
	}
	reportCalls := 0
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[1] == "surface.report_tty" {
			reportCalls++
		}
	}
	if reportCalls != 6 {
		t.Fatalf("report_tty calls = %d, want three attempts per pass proving retry", reportCalls)
	}
	if !containsSubstring(logs, "cmux eviction failed") {
		t.Fatalf("logs = %v, want partial eviction failure warning", logs)
	}
}

func TestCmuxInventoryWarnsWhenSuccessfulEvictionRebuildStaysAmbiguous(t *testing.T) {
	skipCmuxNonDarwin(t)
	// Explicit process_alive:false still queues the corpse without treating it as
	// a live claimant. The remaining uncontested surface now requires kernel
	// liveness (B28); that probe does not weaken the corpse-eviction contract.
	corpse := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	initial := cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": corpse, "tty": "ttys011", "process_alive": "false"},
	)
	rebuilt := cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": corpse, "tty": "ttys011"},
	)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: initial},
		{output: initial}, // CAS: still the same corpse identity
		{},
		{output: rebuilt},
	}}
	var logs []string
	inventory, err := (Cmux{
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: liveOwner,
		Logf:              captureLogf(&logs),
	}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID); err == nil {
		t.Fatal("OwnershipKey() error = nil, want fail-closed stale rebuild")
	}
	if !containsSubstring(logs, "remained ambiguous after corpse eviction") {
		t.Fatalf("logs = %v, want stale-rebuild warning", logs)
	}
}

func TestCmuxInventoryFailsClosedWhenLivenessProbeErrors(t *testing.T) {
	skipCmuxNonDarwin(t)
	// A liveness-probe error is never grounds to evict: fail closed for the tty
	// and log the degraded WARN.
	second := "B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2"
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		map[string]string{"id": second, "tty": "ttys011"},
	)}
	liveness := &fakeTTYLiveness{err: errors.New("sysctl boom")}
	var logs []string
	inventory, err := (Cmux{
		Runner:            runner,
		Path:              "/fake/cmux",
		LiveTTYOwnerCount: liveness.ownerCount,
		Logf:              captureLogf(&logs),
	}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID); err == nil || !strings.Contains(err.Error(), "2 live surface aliases") {
		t.Fatalf("OwnershipKey() error = %v, want fail-closed ambiguity", err)
	}
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[1] == "surface.report_tty" {
			t.Fatalf("unexpected eviction after liveness error: %#v", call)
		}
	}
	if !containsSubstring(logs, "tty-liveness unavailable") {
		t.Fatalf("logs = %v, want degraded WARN", logs)
	}
}

func TestCmuxInventoryEvictsSingleStaleClaimant(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		)},
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
		)},
		{},
		{output: cmuxTreeWithSurfaceRecords(
			map[string]string{"id": testCmuxSurfaceID, "tty": cmuxEvictedTTYName},
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
	if _, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("OwnershipKey() error = %v, want ErrTargetNotFound", err)
	}
	assertCmuxEviction(t, runner, testCmuxSurfaceID)
}

func TestCmuxInventoryDegradesUncontestedClaimantWhenLivenessProbeErrors(t *testing.T) {
	skipCmuxNonDarwin(t)
	runner := &fakeCommandRunner{output: cmuxTreeWithSurfaceRecords(
		map[string]string{"id": testCmuxSurfaceID, "tty": "ttys011"},
	)}
	var logs []string
	inventory, err := (Cmux{
		Runner: runner,
		Path:   "/fake/cmux",
		LiveTTYOwnerCount: func(string) (int, error) {
			return 0, errors.New("sysctl boom")
		},
		Logf: captureLogf(&logs),
	}).Inventory(context.Background(), OwnershipContext{})
	if err != nil {
		t.Fatalf("Inventory() error = %v", err)
	}
	if _, err := inventory.OwnershipKey("cmux:surface:" + testCmuxSurfaceID); !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("OwnershipKey() error = %v, want ErrTargetDegraded", err)
	}
	for _, call := range runner.calls {
		if len(call.args) >= 2 && call.args[1] == "surface.report_tty" {
			t.Fatalf("unexpected eviction after liveness error: %#v", call)
		}
	}
	if !containsSubstring(logs, "tty-liveness unavailable") {
		t.Fatalf("logs = %v, want degraded WARN", logs)
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

func containsSubstring(list []string, want string) bool {
	for _, v := range list {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
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

// captureLogf returns a Logf seam plus a pointer to the accumulated messages.
func captureLogf(messages *[]string) func(string, ...any) {
	return func(format string, args ...any) {
		*messages = append(*messages, fmt.Sprintf(format, args...))
	}
}

func trustedCmux(id string) OwnershipContext {
	return OwnershipContext{TrustedTarget: cmuxSurfaceTargetPrefix + id}
}
