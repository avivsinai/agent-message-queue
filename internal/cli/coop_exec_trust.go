//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// wakeDialogStartupEvent marks a .wake.dialog that coop exec writes before
// it execs a harness that will ask whether to trust its directory. A
// doorbell at that dialog ends the session: Claude Code's Enter picks the
// default "No, exit", and Codex quits. The marker closes when the harness's
// SessionStart hook removes it, or when the harness records the trust
// decision (startupTrustDialogShows turns false), whichever comes first.
const wakeDialogStartupEvent = "Startup"

// startupTrustDialog returns the harness and directory of the trust dialog
// a harness started as binaryPath with args in wd will show, if AMQ can
// tell that it will. Every doubt reads as no dialog, which is the behavior
// without a guard; it never holds the doorbell for a dialog that does not
// show.
func startupTrustDialog(binaryPath string, args []string, wd string) (harness, dir string, ok bool) {
	switch launch.ProviderForExecutable(binaryPath) {
	case launch.ClaudeProvider:
		// Print mode runs no interactive session and asks nothing.
		for _, arg := range args {
			if arg == "--" {
				break
			}
			if arg == "-p" || arg == "--print" {
				return "", "", false
			}
		}
		harness, dir = launch.ClaudeProvider, wd
	case launch.CodexProvider:
		// A subcommand other than a TUI resume or fork runs no TUI start,
		// and --remote checks trust on an app-server AMQ does not see.
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if arg == "--" || !strings.HasPrefix(arg, "-") {
				if arg != "--" && codexSubcommands[arg] && arg != "resume" && arg != "fork" {
					return "", "", false
				}
				break
			}
			if flag, _, inline := strings.Cut(arg, "="); codexValueOptions[flag] && !inline {
				i++
			}
		}
		if codexArgsHaveRemote(args) {
			return "", "", false
		}
		harness, dir = launch.CodexProvider, codexLaunchDir(args, wd)
	default:
		return "", "", false
	}
	return harness, dir, startupTrustDialogShows(harness, dir)
}

// startupTrustDialogShows reports whether harness still asks to trust dir:
// the harness's own record of the decision has no entry for it.
func startupTrustDialogShows(harness, dir string) bool {
	switch harness {
	case launch.ClaudeProvider:
		return claudeTrustDialogShows(dir)
	case launch.CodexProvider:
		return codexTrustDialogShows(dir)
	}
	return false
}

// codexTrustDialogShows reads the user config.toml the way the Codex TUI
// decides its startup trust screen (codex-cli 0.163
// tui/src/config_update.rs read_remote_project_trust): a directory in no
// git checkout is projectless and asks nothing, and a [projects] entry for
// the directory or its git trust root records a decision. A saved
// "untrusted" entry still shows a restricted dialog in Codex; it reads as
// no dialog here, because AMQ cannot see when that one closes.
func codexTrustDialogShows(dir string) bool {
	if codexNearestGitAncestor(dir) == "" {
		return false
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return false
	}
	level, recorded, err := codexProjectTrust(codexHome, dir)
	return err == nil && (!recorded || level == "")
}

// claudeTrustDialogShows reads Claude Code's global config
// ($CLAUDE_CONFIG_DIR or the home directory, .claude.json): the dialog
// shows until projects["<dir>"].hasTrustDialogAccepted is true for dir or
// an ancestor inside its git checkout. Seen live on Claude Code 2.1.308: a
// subdirectory of a trusted checkout asks nothing, while a new checkout
// below a trusted non-git directory asks. Outside a checkout every ancestor
// counts, which reads as no dialog where in doubt.
func claudeTrustDialogShows(dir string) bool {
	configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		configDir = home
	}
	raw, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		return false
	}
	var config struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return false
	}
	top := codexNearestGitAncestor(dir)
	for _, start := range codexTrustKeys(dir) {
		for d := start; ; d = filepath.Dir(d) {
			if config.Projects[d].HasTrustDialogAccepted {
				return false
			}
			if d == top || filepath.Dir(d) == d {
				break
			}
		}
	}
	return true
}

// writeStartupDialogMarker writes the Startup marker for the trust dialog
// the harness about to start will show, so the wake holds the doorbell
// until the dialog is answered. With no dialog it removes a Startup marker
// an earlier launch left (one declined, or one that never started), which
// would otherwise hold this session's doorbell. A failed write leaves the
// behavior without a guard.
func writeStartupDialogMarker(root, handle, binaryPath string, args []string) {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	harness, dir, shows := startupTrustDialog(binaryPath, args, wd)
	agentDir, err := openExistingWakeAgentDir(root, handle)
	if err != nil {
		return
	}
	defer func() { _ = agentDir.Close() }()
	_ = agentDir.withFD(func(dirfd int) error {
		if !shows {
			current, _, exists, err := readWakeRepairMetadataAt(dirfd, wakeDialogFileName, "wake dialog marker",
				filepath.Join(agentDir.path, wakeDialogFileName), maxWakeMetadataFileBytes)
			var marker wakeDialogMarker
			if err != nil || !exists || json.Unmarshal(current, &marker) != nil || marker.Event != wakeDialogStartupEvent {
				return nil
			}
			return clearWakeDialogMarker(dirfd, agentDir, "")
		}
		raw, err := json.Marshal(wakeDialogMarker{
			Schema: 1, Event: wakeDialogStartupEvent, Harness: harness, Cwd: dir,
			OpenedAt: time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			return err
		}
		return replaceWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'))
	})
}

// startupDialogClosed reports whether marker is a Startup marker whose
// trust dialog the harness has since recorded an answer for. cache keeps
// the last answer while the harness's trust file is unchanged, so a held
// wake does not reparse it on every recheck.
func startupDialogClosed(raw []byte, cache *startupDialogCache) bool {
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent ||
		!slices.Contains([]string{launch.ClaudeProvider, launch.CodexProvider}, marker.Harness) || !filepath.IsAbs(marker.Cwd) {
		return false
	}
	stamp := startupTrustFileStamp(marker.Harness)
	if cache.valid && cache.harness == marker.Harness && cache.cwd == marker.Cwd && cache.stamp == stamp {
		return cache.closed
	}
	closed := !startupTrustDialogShows(marker.Harness, marker.Cwd)
	*cache = startupDialogCache{valid: true, harness: marker.Harness, cwd: marker.Cwd, stamp: stamp, closed: closed}
	return closed
}

type startupDialogCache struct {
	valid        bool
	harness, cwd string
	stamp        string
	closed       bool
}

// startupTrustFileStamp identifies the current version of the file where
// harness records trust: its path, size and mtime, or "" if unreadable.
func startupTrustFileStamp(harness string) string {
	var path string
	switch harness {
	case launch.ClaudeProvider:
		configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
		if configDir == "" {
			configDir, _ = os.UserHomeDir()
		}
		path = filepath.Join(configDir, ".claude.json")
	case launch.CodexProvider:
		codexHome, err := codexHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(codexHome, "config.toml")
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s\x00%d\x00%d", path, info.ModTime().UnixNano(), info.Size())
}
