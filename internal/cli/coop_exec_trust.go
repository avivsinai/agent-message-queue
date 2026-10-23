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

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// wakeDialogStartupEvent marks a .wake.dialog that coop exec writes before
// it execs a harness that will ask whether to trust its directory. A
// doorbell at that dialog ends the session: Claude Code's Enter picks the
// default "No, exit", and Codex quits. The marker closes when the harness's
// SessionStart hook removes it, or when the harness records that it trusts
// the directory, whichever comes first.
const wakeDialogStartupEvent = "Startup"

// startupTrust is what the harness's own trust record says about a
// directory.
type startupTrust uint8

const (
	// startupTrustUnknown: the record cannot be read or does not decide.
	// coop exec writes no marker for it, and an open marker stays open.
	startupTrustUnknown startupTrust = iota
	// startupTrustPending: the harness will show (or still shows) its
	// trust dialog for the directory.
	startupTrustPending
	// startupTrustAccepted: the harness records that it trusts the
	// directory, the only evidence that releases a Startup marker.
	startupTrustAccepted
)

// startupTrustDialog returns the harness and directory a harness started
// as binaryPath with args in wd checks trust for, and what its record says
// now. A launch that shows no trust dialog reads as unknown.
func startupTrustDialog(binaryPath string, args []string, wd string) (harness, dir string, trust startupTrust) {
	switch launch.ProviderForExecutable(binaryPath) {
	case launch.ClaudeProvider:
		// Print mode runs no interactive session and asks nothing.
		for _, arg := range args {
			if arg == "--" {
				break
			}
			if arg == "-p" || arg == "--print" {
				return "", "", startupTrustUnknown
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
					return "", "", startupTrustUnknown
				}
				break
			}
			if flag, _, inline := strings.Cut(arg, "="); codexValueOptions[flag] && !inline {
				i++
			}
		}
		if codexArgsHaveRemote(args) {
			return "", "", startupTrustUnknown
		}
		harness, dir = launch.CodexProvider, codexLaunchDir(args, wd)
	default:
		return "", "", startupTrustUnknown
	}
	return harness, dir, readStartupTrust(harness, dir)
}

func readStartupTrust(harness, dir string) startupTrust {
	switch harness {
	case launch.ClaudeProvider:
		return claudeStartupTrust(dir)
	case launch.CodexProvider:
		return codexStartupTrust(dir)
	}
	return startupTrustUnknown
}

// codexStartupTrust reads the user config.toml the way the Codex TUI
// decides its startup trust screen (codex-cli 0.163
// tui/src/config_update.rs read_remote_project_trust): a directory in no
// git checkout is projectless and asks nothing; a [projects] entry for the
// directory or its git trust root with trust_level "trusted" asks nothing;
// no entry, or a saved "untrusted" one (a restricted dialog), asks.
func codexStartupTrust(dir string) startupTrust {
	if codexNearestGitAncestor(dir) == "" {
		return startupTrustUnknown
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return startupTrustUnknown
	}
	level, _, err := codexProjectTrust(codexHome, dir)
	switch {
	case err != nil:
		return startupTrustUnknown
	case level == "trusted":
		return startupTrustAccepted
	}
	return startupTrustPending
}

// claudeStartupTrust reads Claude Code's global config ($CLAUDE_CONFIG_DIR
// or the home directory, .claude.json): the dialog shows until
// projects["<dir>"].hasTrustDialogAccepted is true for dir or an ancestor
// inside its git checkout. Seen live on Claude Code 2.1.308: a
// subdirectory of a trusted checkout asks nothing, while a new checkout
// below a trusted non-git directory asks. Outside a checkout every
// ancestor counts, which reads as accepted where in doubt.
func claudeStartupTrust(dir string) startupTrust {
	configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return startupTrustUnknown
		}
		configDir = home
	}
	raw, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		return startupTrustUnknown
	}
	var config struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return startupTrustUnknown
	}
	top := codexNearestGitAncestor(dir)
	for _, start := range codexTrustKeys(dir) {
		for d := start; ; d = filepath.Dir(d) {
			if config.Projects[d].HasTrustDialogAccepted {
				return startupTrustAccepted
			}
			if d == top || filepath.Dir(d) == d {
				break
			}
		}
	}
	return startupTrustPending
}

// writeStartupDialogMarker writes the Startup marker when the harness about
// to start will show its trust dialog, so the wake holds the doorbell until
// the dialog is answered. A failed write leaves the behavior without a
// guard.
func writeStartupDialogMarker(root, handle, binaryPath string, args []string) {
	withStartupDialog(root, handle, binaryPath, args, func(dirfd int, agentDir *wakeAgentDir, harness, dir string, trust startupTrust) error {
		if trust != startupTrustPending {
			return nil
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

// clearStaleStartupDialogMarker removes a Startup marker an earlier launch
// left (one declined, or one that never started) when this launch's harness
// shows no trust dialog; it would otherwise hold this session's doorbell.
// The caller owns the agent's wake.
func clearStaleStartupDialogMarker(root, handle, binaryPath string, args []string) {
	withStartupDialog(root, handle, binaryPath, args, func(dirfd int, agentDir *wakeAgentDir, _, _ string, trust startupTrust) error {
		if trust == startupTrustPending {
			return nil
		}
		current, _, exists, err := readWakeRepairMetadataAt(dirfd, wakeDialogFileName, "wake dialog marker",
			filepath.Join(agentDir.path, wakeDialogFileName), maxWakeMetadataFileBytes)
		var marker wakeDialogMarker
		if err != nil || !exists || json.Unmarshal(current, &marker) != nil || marker.Event != wakeDialogStartupEvent {
			return nil
		}
		return clearWakeDialogMarker(dirfd, agentDir, "")
	})
}

func withStartupDialog(root, handle, binaryPath string, args []string, fn func(dirfd int, agentDir *wakeAgentDir, harness, dir string, trust startupTrust) error) {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	harness, dir, trust := startupTrustDialog(binaryPath, args, wd)
	agentDir, err := openExistingWakeAgentDir(root, handle)
	if err != nil {
		return
	}
	defer func() { _ = agentDir.Close() }()
	_ = agentDir.withFD(func(dirfd int) error {
		return fn(dirfd, agentDir, harness, dir, trust)
	})
}

// startupDialogClosed reports whether marker is a Startup marker whose
// harness now records that it trusts the directory. A record that cannot be
// read or does not decide keeps the marker open. cache keeps the last
// answer while the harness's trust file is unchanged, so a held wake does
// not reparse it on every recheck.
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
	closed := readStartupTrust(marker.Harness, marker.Cwd) == startupTrustAccepted
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

// codexStartupHoldHint reports an open Codex Startup marker for handle. Codex
// records no decision for a directory the person keeps untrusted, so
// without the Codex dialog hook's SessionStart close that hold releases
// only by aging: input held, attention output after wakeDialogQuietHold.
func codexStartupHoldHint(root, handle string) (opsHint, bool) {
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(fsq.AgentBase(root, handle), wakeDialogFileName), maxWakeMetadataFileBytes)
	var marker wakeDialogMarker
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent || marker.Harness != launch.CodexProvider {
		return opsHint{}, false
	}
	return opsHint{
		Code:   "codex_trust_hold",
		Status: "warn",
		Message: fmt.Sprintf("%s's doorbell is held while Codex asks whether to trust %s; it rings once Codex records trust, or with the Codex dialog hook when the session starts; remedy: amq wake install-dialog-hook --harness codex",
			handle, marker.Cwd),
	}, true
}
