package launch

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	PlacementTargetCurrentWindow = "current_window"
	PlacementTargetNewWindow     = "new_window"
	PlacementTargetSession       = "session"

	PlacementLayoutColumns = "columns"
	PlacementLayoutRows    = "rows"
	PlacementLayoutTiled   = "tiled"

	PlacementUnsupportedReason = "placement_unsupported"
	maxPlacementStaggerMS      = 60_000
	maxLauncherPaneBytes       = 256
)

// Placement is the internal complete-tuple form of public PlacementV1.
// launchapi wiring lands with ul7; backends consume this type directly.
type Placement struct {
	Target       string `json:"target"`
	Layout       string `json:"layout"`
	StaggerMS    int    `json:"stagger_ms,omitempty"`
	LauncherPane string `json:"launcher_pane,omitempty"`
}

// PlacementPreview is the internal form of public PlacementPreviewV1.
type PlacementPreview struct {
	Requested  *Placement `json:"requested,omitempty"`
	Effective  Placement  `json:"effective"`
	Supported  bool       `json:"supported"`
	ReasonCode string     `json:"reason_code,omitempty"`
}

func (p Placement) Validate() error {
	switch p.Target {
	case PlacementTargetCurrentWindow, PlacementTargetNewWindow, PlacementTargetSession:
	default:
		return fmt.Errorf("invalid placement target %q", p.Target)
	}
	switch p.Layout {
	case PlacementLayoutColumns, PlacementLayoutRows, PlacementLayoutTiled:
	default:
		return fmt.Errorf("invalid placement layout %q", p.Layout)
	}
	if p.StaggerMS < 0 || p.StaggerMS > maxPlacementStaggerMS {
		return fmt.Errorf("stagger_ms must be between 0 and %d", maxPlacementStaggerMS)
	}
	if p.LauncherPane != "" {
		if p.Target != PlacementTargetCurrentWindow {
			return fmt.Errorf("launcher_pane is valid only with target %q", PlacementTargetCurrentWindow)
		}
		if !utf8.ValidString(p.LauncherPane) || strings.ContainsRune(p.LauncherPane, 0) ||
			len(p.LauncherPane) > maxLauncherPaneBytes {
			return fmt.Errorf("launcher_pane must be at most %d UTF-8 bytes without NUL", maxLauncherPaneBytes)
		}
	}
	return nil
}

// LegacyPlacement is each backend's v0.61 omitted-placement behavior.
func LegacyPlacement(backend string) Placement {
	switch backend {
	case LauncherTMux:
		return Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}
	case LauncherCMux:
		return Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns}
	case LauncherGhostty:
		return Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns}
	default:
		return Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}
	}
}

// PlacementSupported reports whether the backend can realize the complete
// requested tuple. The table is the amq-94w safe set; it is not inferred
// field-by-field.
func PlacementSupported(backend string, requested Placement) bool {
	if err := requested.Validate(); err != nil {
		return false
	}
	switch backend {
	case LauncherTMux:
		switch requested.Target {
		case PlacementTargetSession, PlacementTargetNewWindow:
			return requested.LauncherPane == "" && supportedTmuxLayout(requested.Layout)
		case PlacementTargetCurrentWindow:
			return tmuxPaneID(requested.LauncherPane) && supportedTmuxLayout(requested.Layout)
		}
	case LauncherGhostty:
		return requested.Target == PlacementTargetNewWindow &&
			requested.LauncherPane == "" &&
			(requested.Layout == PlacementLayoutColumns || requested.Layout == PlacementLayoutRows)
	case LauncherCMux:
		return requested.Target == PlacementTargetCurrentWindow &&
			requested.LauncherPane == "" &&
			(requested.Layout == PlacementLayoutColumns || requested.Layout == PlacementLayoutRows)
	}
	return false
}

func ResolvePlacement(backend string, requested *Placement) (PlacementPreview, error) {
	if requested == nil {
		return PlacementPreview{Effective: LegacyPlacement(backend), Supported: true}, nil
	}
	if err := requested.Validate(); err != nil {
		return PlacementPreview{}, err
	}
	preview := PlacementPreview{Requested: clonePlacement(requested), Effective: *requested, Supported: PlacementSupported(backend, *requested)}
	if !preview.Supported {
		preview.ReasonCode = PlacementUnsupportedReason
	}
	return preview, nil
}

func clonePlacement(p *Placement) *Placement {
	if p == nil {
		return nil
	}
	copied := *p
	return &copied
}

func supportedTmuxLayout(layout string) bool {
	return layout == PlacementLayoutColumns || layout == PlacementLayoutRows || layout == PlacementLayoutTiled
}

func tmuxPaneID(pane string) bool {
	if len(pane) < 2 || pane[0] != '%' {
		return false
	}
	for _, r := range pane[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func tmuxSelectLayout(layout string) string {
	switch layout {
	case PlacementLayoutRows:
		return "even-vertical"
	case PlacementLayoutTiled:
		return "tiled"
	default:
		return "even-horizontal"
	}
}

func tmuxSplitFlag(layout string) string {
	if layout == PlacementLayoutRows {
		return "-v"
	}
	return "-h"
}

func cmuxSplitDirection(layout string) string {
	if layout == PlacementLayoutRows {
		return "down"
	}
	return "right"
}

func ghosttySplitDirection(layout string) string {
	if layout == PlacementLayoutRows {
		return "down"
	}
	return "right"
}

func placementError(reason string) error {
	return fmt.Errorf("%s", reason)
}

func resolveCreatePlacement(backend string, req CreateRequest) (PlacementPreview, error) {
	preview, err := ResolvePlacement(backend, req.Placement)
	if err != nil {
		return PlacementPreview{}, err
	}
	if req.Placement != nil && !preview.Supported {
		return preview, placementError(PlacementUnsupportedReason)
	}
	return preview, nil
}

func placementStaggerDuration(p *Placement) time.Duration {
	if p == nil || p.StaggerMS <= 0 {
		return 0
	}
	return time.Duration(p.StaggerMS) * time.Millisecond
}

func placementStaggerBudget(p *Placement, agents int, includeFirst bool) time.Duration {
	delay := placementStaggerDuration(p)
	if delay == 0 || agents <= 0 {
		return 0
	}
	n := agents - 1
	if includeFirst {
		n = agents
	}
	if n < 0 {
		return 0
	}
	return time.Duration(n) * delay
}

func journalLayoutTarget(journal LaunchJournal) string {
	if journal.Placement.Effective.Target != "" {
		return journal.Placement.Effective.Target
	}
	return PlacementTargetSession
}
