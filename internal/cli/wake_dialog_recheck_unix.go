//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// codexStartupRecheckEvery paces the wake's startup probe while a Codex
// Startup marker is open.
const codexStartupRecheckEvery = 10 * time.Second

// wakeStartupRecheck is the wake's proof-based release of a Codex Startup
// marker. A marker that only the dialog hook would otherwise remove
// (update_prompt, hook_review) is removed once Codex's own state proves
// what coop exec could not: the directory trust recorded, no hook to
// review, and the update prompt ruled out. That needs no hook, and nothing
// is released on a timer. The dialog source and the trust-release check
// of one wake share it.
type wakeStartupRecheck struct {
	lastProbe time.Time
	// released reports a marker this wake removed on that proof. The
	// proof shows the screens are gone, not that Codex accepts input yet,
	// so a written doorbell is not presented until a drain proves it.
	released bool
}

// releaseCodexStartup removes the Codex Startup marker raw, through
// agentDir, when Codex's state proves its startup screens closed. It
// reports whether it removed it; a marker replaced meanwhile stays.
func (r *wakeStartupRecheck) releaseCodexStartup(agentDir *wakeAgentDir, raw []byte, now time.Time) bool {
	if r == nil || !r.codexStartupProven(raw, now) {
		return false
	}
	removed := false
	err := agentDir.withFD(func(dirfd int) error {
		return removeWakeDialogMarkerIf(dirfd, agentDir, func(current []byte) bool {
			removed = bytes.Equal(current, raw)
			return removed
		})
	})
	if err != nil || !removed {
		return false
	}
	r.released = true
	return true
}

// codexStartupProven decides the release the way coop exec decides the
// marker (startupDialogMarkerFor), from the plan the marker keeps. The
// probe runs at most once per codexStartupRecheckEvery, only once the
// trust is recorded, and outside every lock: the caller holds none.
func (r *wakeStartupRecheck) codexStartupProven(raw []byte, now time.Time) bool {
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent || marker.Harness != launch.CodexProvider ||
		(marker.Kind != wakeStartupKindUpdate && marker.Kind != wakeStartupKindReview) || marker.Probe == nil || !filepath.IsAbs(marker.Cwd) {
		return false
	}
	plan := *marker.Probe
	if !filepath.IsAbs(plan.Binary) || !filepath.IsAbs(plan.CodexHome) || !filepath.IsAbs(plan.Dir) ||
		launch.ProviderForExecutable(plan.Binary) != launch.CodexProvider {
		return false
	}
	if codexStartupTrustIn(plan.CodexHome, marker.Cwd) != startupTrustAccepted {
		return false
	}
	installed, known := parseCodexVersion(plan.Version)
	verified := known && codexUpdateVersionVerified(installed)
	cacheRuledOut := verified && codexVersionCacheRulesOutPrompt(plan.CodexHome, installed)
	// A verified release before hook review shows no hook review.
	preReview := verified && codexVersionNewer(codexHookReviewSince, installed)
	if preReview && cacheRuledOut {
		return true
	}
	if !r.lastProbe.IsZero() && !now.Before(r.lastProbe) && now.Sub(r.lastProbe) < codexStartupRecheckEvery {
		return false
	}
	r.lastProbe = now
	ctx, cancel := context.WithTimeout(context.Background(), codexHooksProbeTimeout)
	defer cancel()
	probe := runCodexStartupProbe(ctx, plan)
	return (preReview || probe.hooksClear) && (cacheRuledOut || probe.updateCheckOff && probe.requirementsClear)
}
