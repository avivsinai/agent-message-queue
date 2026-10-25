//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Codex shows its update prompt before its trust dialog and before its
// session starts, and the prompt's default choice runs the upgrade
// (tui/src/lib.rs run_ratatui_app: run_update_prompt_if_needed precedes
// start_app_server). Choosing "Not now" writes nothing, so no Codex record
// shows that the prompt closed. coop exec therefore holds the doorbell
// unless Codex's own state proves the prompt cannot show:
// codexUpdatePromptRuledOut. The decision mirrors codex-cli source at
// rust-v0.145.0 and rust-v0.161.0 and main of 2026-10-07
// (tui/src/updates.rs get_upgrade_version_for_popup and get_upgrade_version,
// tui/src/updates_cache.rs, tui/src/update_versions.rs is_newer and
// parse_version, tui/src/update_prompt.rs run_update_prompt_if_needed),
// and codex-cli 0.163.0 as installed. Whatever AMQ cannot read or parse
// keeps the hold.

const (
	// codexManagedConfigPath is the legacy managed config on Unix, which
	// Codex reads both as a config layer above the CLI overrides and as
	// requirements (config/src/loader/layer_io.rs).
	codexManagedConfigPath = "/etc/codex/managed_config.toml"
	// codexSystemConfigPath is the system config layer, below the user's.
	codexSystemConfigPath = "/etc/codex/config.toml"
	// codexManagedPreferences is where macOS keeps forced managed
	// preferences; Codex reads the com.openai.codex domain
	// (config/src/loader/macos.rs). AMQ does not decode them.
	codexManagedPreferences = "/Library/Managed Preferences"
	codexManagedDomainFile  = "com.openai.codex.plist"

	codexVersionFile          = "version.json"
	codexVersionProbeTimeout  = 3 * time.Second
	codexVersionProbeMaxBytes = 4 << 10
)

// codexVerifiedMinor bounds the codex-cli 0.x releases whose update
// decision AMQ read; any other version keeps the hold.
const (
	codexVerifiedMinorLow  = 145
	codexVerifiedMinorHigh = 163
)

// codexUpdatePromptRuledOut reports whether a Codex started as binaryPath
// with args in dir provably shows no update prompt: the update check is off
// in its effective configuration, or its version cache names no newer
// release than the installed one, or a dismissed one.
func codexUpdatePromptRuledOut(binaryPath string, args []string, dir string) bool {
	codexHome, err := codexHomeDir()
	if err != nil {
		return false
	}
	codexHome = absPath(filepath.Clean(codexHome))
	return codexUpdateCheckOff(codexHome, dir, args) || codexVersionCacheRulesOutPrompt(binaryPath, codexHome)
}

// codexUpdateCheckOff reports whether check_for_update_on_startup is
// provably false for this start (config/src/loader/README.md layer order:
// managed config, then CLI overrides, project, user profile, user, system;
// requirements pin an exact value above all). A layer that turns the check
// on, or one AMQ cannot read, defeats the proof. A project or profile layer
// can veto but never proves: Codex disables an untrusted project's layer,
// and only the selected profile counts.
func codexUpdateCheckOff(codexHome, dir string, args []string) bool {
	if codexManagedPreferencesPresent() {
		return false
	}
	for _, path := range []string{codexRequirementsPath, codexManagedConfigPath} {
		off, set, err := codexConfigUpdateCheck(path)
		if err != nil {
			return false
		}
		if set {
			return off
		}
	}
	vetoes := []string{}
	for d := dir; ; d = filepath.Dir(d) {
		vetoes = append(vetoes, filepath.Join(d, ".codex", "config.toml"))
		if filepath.Dir(d) == d {
			break
		}
	}
	profiles, err := filepath.Glob(filepath.Join(codexHome, "*.config.toml"))
	if err != nil {
		return false
	}
	for _, path := range append(vetoes, profiles...) {
		off, set, err := codexConfigUpdateCheck(path)
		if err != nil || (set && !off) {
			return false
		}
	}
	if value, set := codexArgsUpdateCheck(args); set {
		return value == "false"
	}
	for _, path := range []string{filepath.Join(codexHome, "config.toml"), codexSystemConfigPath} {
		off, set, err := codexConfigUpdateCheck(path)
		if err != nil {
			return false
		}
		if set {
			return off
		}
	}
	return false
}

// codexManagedPreferencesPresent reports a forced com.openai.codex domain,
// machine-wide or for this user, or a state AMQ cannot inspect.
func codexManagedPreferencesPresent() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	current, err := user.Current()
	if err != nil || current.Username == "" {
		return true
	}
	for _, path := range []string{
		filepath.Join(codexManagedPreferences, codexManagedDomainFile),
		filepath.Join(codexManagedPreferences, current.Username, codexManagedDomainFile),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// codexConfigUpdateCheck reads the top-level check_for_update_on_startup
// of one TOML file. A missing file sets nothing; a value that is not a
// boolean is an error.
func codexConfigUpdateCheck(path string) (off, set bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var config map[string]any
	if err := toml.Unmarshal(raw, &config); err != nil {
		return false, false, err
	}
	value, set := config[codexUpdateCheckKey]
	if !set {
		return false, false, nil
	}
	enabled, ok := value.(bool)
	if !ok {
		return false, false, errors.New(codexUpdateCheckKey + " is not a boolean")
	}
	return !enabled, true, nil
}

// codexArgsUpdateCheck returns the value the last -c override of the update
// check sets, trimmed.
func codexArgsUpdateCheck(args []string) (value string, set bool) {
	for _, override := range codexConfigOverrides(args) {
		key, v, _ := strings.Cut(override, "=")
		if strings.EqualFold(strings.TrimSpace(key), codexUpdateCheckKey) {
			value, set = strings.TrimSpace(v), true
		}
	}
	return value, set
}

// codexVersionCacheRulesOutPrompt reports whether Codex's version cache
// rules the prompt out for the installed binary: its latest_version is not
// newer than the installed version (update_versions.rs is_newer), or it is
// the dismissed version. A missing or unparseable cache, a version Codex
// itself cannot order, or an installed version outside the verified range
// keeps the hold.
func codexVersionCacheRulesOutPrompt(binaryPath, codexHome string) bool {
	raw, err := os.ReadFile(filepath.Join(codexHome, codexVersionFile))
	if err != nil {
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
	if _, err := time.Parse(time.RFC3339Nano, *cache.Checked); err != nil {
		return false
	}
	latest, ok := parseCodexVersion(*cache.Latest)
	if !ok {
		return false
	}
	installed, ok := codexInstalledVersion(binaryPath)
	if !ok || installed[0] != 0 || installed[1] < codexVerifiedMinorLow || installed[1] > codexVerifiedMinorHigh {
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
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil || len(out) > codexVersionProbeMaxBytes {
		return [3]uint64{}, false
	}
	version, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "codex-cli ")
	if !ok {
		return [3]uint64{}, false
	}
	return parseCodexVersion(version)
}
