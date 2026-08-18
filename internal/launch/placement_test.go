package launch

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPlacementCapabilitiesSafeSet(t *testing.T) {
	tmuxPane := Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, LauncherPane: "%12"}
	cases := []struct {
		backend   string
		placement Placement
		want      bool
	}{
		{LauncherTMux, Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}, true},
		{LauncherTMux, Placement{Target: PlacementTargetSession, Layout: PlacementLayoutRows}, true},
		{LauncherTMux, Placement{Target: PlacementTargetSession, Layout: PlacementLayoutTiled}, true},
		{LauncherTMux, Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns}, true},
		{LauncherTMux, Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutRows}, true},
		{LauncherTMux, Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutTiled}, true},
		{LauncherTMux, tmuxPane, true},
		{LauncherTMux, Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutTiled, LauncherPane: "%0"}, true},
		{LauncherTMux, Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns}, false},
		{LauncherTMux, Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, LauncherPane: "surface:1"}, false},
		{LauncherGhostty, Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns}, true},
		{LauncherGhostty, Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutRows}, true},
		{LauncherGhostty, Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutTiled}, false},
		{LauncherGhostty, Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}, false},
		{LauncherGhostty, Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, LauncherPane: "%1"}, false},
		{LauncherCMux, Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns}, true},
		{LauncherCMux, Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutRows}, true},
		{LauncherCMux, Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutTiled}, false},
		{LauncherCMux, Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, LauncherPane: "%12"}, false},
		{LauncherCMux, Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}, false},
		{CommandsBackendName, Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}, false},
		{CommandsBackendName, Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns}, false},
	}
	for _, tc := range cases {
		if got := PlacementSupported(tc.backend, tc.placement); got != tc.want {
			t.Errorf("PlacementSupported(%s, %+v) = %v, want %v", tc.backend, tc.placement, got, tc.want)
		}
	}
}

func TestResolvePlacementOmittedUsesLegacy(t *testing.T) {
	for _, backend := range []string{LauncherTMux, LauncherCMux, LauncherGhostty, CommandsBackendName} {
		preview, err := ResolvePlacement(backend, nil)
		if err != nil || !preview.Supported || preview.Requested != nil {
			t.Fatalf("%s omitted preview = %#v, %v", backend, preview, err)
		}
		if preview.Effective != LegacyPlacement(backend) {
			t.Fatalf("%s effective = %+v, want %+v", backend, preview.Effective, LegacyPlacement(backend))
		}
	}
}

func TestPlacementValidateRejectsInvalidCombinations(t *testing.T) {
	err := (Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns, LauncherPane: "%1"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "launcher_pane") {
		t.Fatalf("session+launcher error = %v", err)
	}
	err = (Placement{Target: PlacementTargetCurrentWindow, Layout: "diagonal"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "layout") {
		t.Fatalf("bad layout error = %v", err)
	}
	err = (Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns, StaggerMS: 60_001}).Validate()
	if err == nil || !strings.Contains(err.Error(), "stagger_ms") {
		t.Fatalf("stagger error = %v", err)
	}
}

func TestCommandsCreateRefusesExplicitPlacement(t *testing.T) {
	_, root := openTestRoot(t)
	_, err := Commands{}.Create(CreateRequest{
		Session: "collab", Plan: validPlan(), Root: root,
		Placement: &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns},
	})
	var definite *DefinitePreCreateError
	if err == nil || !errors.As(err, &definite) || !strings.Contains(err.Error(), PlacementUnsupportedReason) {
		t.Fatalf("explicit commands placement error = %v", err)
	}
}

func TestPlacementStaggerBudgetCoversCreateDeadline(t *testing.T) {
	session := &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns, StaggerMS: 60_000}
	if got := placementStaggerBudget(session, 2, false); got != 60*time.Second {
		t.Fatalf("session budget = %s, want 60s", got)
	}
	current := &Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, StaggerMS: 60_000, LauncherPane: "%0"}
	if got := placementStaggerBudget(current, 2, true); got != 120*time.Second {
		t.Fatalf("current_window budget = %s, want 120s", got)
	}
	req := CreateRequest{
		Plan:      Plan{Agents: []AgentPlan{{Handle: "claude"}, {Handle: "codex"}}},
		Placement: session,
	}
	if got := tmuxCreateTimeout(req); got < tmuxCommandTimeout+60*time.Second {
		t.Fatalf("tmux create timeout = %s, want at least %s", got, tmuxCommandTimeout+60*time.Second)
	}
	req.Placement = current
	if got := tmuxCreateTimeout(req); got < tmuxCommandTimeout+120*time.Second {
		t.Fatalf("tmux current_window timeout = %s, want at least %s", got, tmuxCommandTimeout+120*time.Second)
	}
}
