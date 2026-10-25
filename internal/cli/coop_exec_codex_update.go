//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	codexVersionFile         = "version.json"
	codexVersionCacheFresh   = 19 * time.Hour
	codexVersionProbeTimeout = 3 * time.Second
)

// A nearby release or a different patch is not verified evidence.
func codexUpdateVersionVerified(version [3]uint64) bool {
	return version == [3]uint64{0, 145, 0} || version == [3]uint64{0, 161, 0} || version == [3]uint64{0, 163, 0} ||
		version == [3]uint64{0, 164, 0}
}

// codexHookReviewSince is the first codex-cli release whose TUI reviews new
// or changed hooks after its trust screen and before the session starts
// (tui/src/lib.rs:2121-2182 at rust-v0.164.0). Directory trust does not
// answer that review.
var codexHookReviewSince = [3]uint64{0, 164, 0}

// Codex shows its update prompt before its trust dialog and before its
// session starts, and the prompt's default choice runs the upgrade
// (tui/src/lib.rs run_ratatui_app: run_update_prompt_if_needed precedes
// start_app_server). Choosing "Not now" writes nothing, so no Codex record
// shows that the prompt closed. coop exec therefore holds the doorbell
// unless Codex's own state proves the prompt cannot show:
// codexUpdatePromptRuledOut reads Codex's version cache. The other proof,
// an effective check_for_update_on_startup of false with no requirement
// that pins the check on, needs the app-server probe
// (startupDialogMarkerFor); a CLI or file setting alone cannot prove the
// effective value above managed layers and requirements.
// Unknown state stays held until Codex's first-turn dialog hook closes the
// marker. installed is the binary's version, known only when its probe
// succeeded.
func codexUpdatePromptRuledOut(installed [3]uint64, known bool) bool {
	if !known || !codexUpdateVersionVerified(installed) {
		return false
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return false
	}
	return codexVersionCacheRulesOutPrompt(absPath(filepath.Clean(codexHome)), installed)
}

// codexVersionCacheRulesOutPrompt reports whether Codex's version cache
// rules the prompt out for the installed binary: its latest_version is not
// newer than the installed version (update_versions.rs is_newer), or it is
// the dismissed version. A missing, unparseable, stale or future-dated
// cache, a version Codex itself cannot order, or an installed version
// outside the verified releases keeps the hold.
func codexVersionCacheRulesOutPrompt(codexHome string, installed [3]uint64) bool {
	raw, err := os.ReadFile(filepath.Join(codexHome, codexVersionFile))
	if err != nil {
		return false
	}
	return codexCachedVersionRulesOutPrompt(raw, installed, time.Now())
}

func codexCachedVersionRulesOutPrompt(raw []byte, installed [3]uint64, now time.Time) bool {
	if !codexUpdateVersionVerified(installed) {
		return false
	}
	var cache struct {
		Latest    *string `json:"latest_version"`
		Checked   *string `json:"last_checked_at"`
		Dismissed *string `json:"dismissed_version"`
	}
	if json.Unmarshal(raw, &cache) != nil || cache.Latest == nil || cache.Checked == nil {
		return false
	}
	checked, err := time.Parse(time.RFC3339Nano, *cache.Checked)
	if err != nil {
		return false
	}
	if age := now.Sub(checked); age < 0 || age >= codexVersionCacheFresh {
		return false
	}
	latest, ok := parseCodexVersion(*cache.Latest)
	if !ok {
		return false
	}
	if cache.Dismissed != nil && *cache.Dismissed == *cache.Latest {
		return true
	}
	return !codexVersionNewer(latest, installed)
}

// parseCodexVersion reads a version the way codex-cli parse_version does:
// the first three dot-separated fields as unsigned integers.
func parseCodexVersion(v string) ([3]uint64, bool) {
	var out [3]uint64
	fields := strings.Split(strings.TrimSpace(v), ".")
	if len(fields) < 3 {
		return out, false
	}
	for i := range out {
		n, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func codexVersionNewer(a, b [3]uint64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// codexInstalledVersion runs binaryPath --version and reads its
// "codex-cli X.Y.Z" line. The probe is killed and reaped at its timeout.
func codexInstalledVersion(binaryPath string) ([3]uint64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), codexVersionProbeTimeout)
	defer cancel()
	output, ok := probeHarnessVersion(ctx, binaryPath)
	if !ok {
		return [3]uint64{}, false
	}
	version, ok := strings.CutPrefix(output, "codex-cli ")
	if !ok || len(strings.Split(version, ".")) != 3 {
		return [3]uint64{}, false
	}
	return parseCodexVersion(version)
}
