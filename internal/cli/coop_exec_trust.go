//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"golang.org/x/sys/unix"
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
		return removeWakeDialogMarkerIf(dirfd, agentDir, func(current []byte) bool {
			var marker wakeDialogMarker
			return json.Unmarshal(current, &marker) == nil && marker.Event == wakeDialogStartupEvent
		})
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

// codexStartupHoldHint reports a Codex Startup marker for handle that still
// holds input (startupDialogClosed, as the wake decides). Codex
// records no decision for a directory the person keeps untrusted, so
// without the Codex dialog hook's SessionStart close that hold releases
// only by aging: input held, attention output after wakeDialogQuietHold.
func codexStartupHoldHint(root, handle string) (opsHint, bool) {
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(fsq.AgentBase(root, handle), wakeDialogFileName), maxWakeMetadataFileBytes)
	var marker wakeDialogMarker
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent || marker.Harness != launch.CodexProvider ||
		startupDialogClosed(raw, &startupDialogCache{}) {
		return opsHint{}, false
	}
	return opsHint{
		Code:   "codex_trust_hold",
		Status: "warn",
		Message: fmt.Sprintf("%s's doorbell is held while Codex asks whether to trust %s; it rings once Codex records trust, or with the Codex dialog hook when the session starts; remedy: amq wake install-dialog-hook --harness codex",
			handle, marker.Cwd),
	}, true
}

// envWakeStartupHold names the descriptor of a coop wake's startup hold:
// the read end of a pipe whose write end coop exec keeps until it has
// installed or cleared the Startup marker. Until the pipe reports EOF the
// wake treats a dialog as open and types nothing. coop exec's exit or exec
// closes the write end too (close-on-exec), so the hold never outlives it.
const envWakeStartupHold = "AMQ_WAKE_STARTUP_HOLD_FD"

// attachWakeStartupHold passes a startup hold pipe to cmd. hold is the end
// coop exec closes to release it; childEnd is closed once cmd started.
func attachWakeStartupHold(cmd *exec.Cmd) (hold, childEnd *os.File, err error) {
	childEnd, hold, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	childFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, childEnd)
	cmd.Env = setEnvVar(unsetEnvVar(cmd.Env, envWakeStartupHold), envWakeStartupHold, strconv.Itoa(childFD))
	return hold, childEnd, nil
}

// wakeStartupHoldFromEnv adopts the startup hold coop exec passed, if any.
// released is closed once coop exec releases it.
func wakeStartupHoldFromEnv() (released <-chan struct{}, err error) {
	raw, present := os.LookupEnv(envWakeStartupHold)
	if !present {
		return nil, nil
	}
	if err := os.Unsetenv(envWakeStartupHold); err != nil {
		return nil, fmt.Errorf("clear %s after ingestion: %w", envWakeStartupHold, err)
	}
	fd, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("%s is invalid", envWakeStartupHold)
	}
	unix.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), "amq-wake-startup-hold")
	if file == nil {
		return nil, fmt.Errorf("%s fd is unavailable", envWakeStartupHold)
	}
	// The descriptor stays open for the life of the process, so a restart
	// can hand it to its successor (carryWakeStartupHold).
	wakeStartupHoldFile = file
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, file)
	}()
	return done, nil
}

// wakeStartupHoldFile is the adopted startup hold, kept referenced so its
// descriptor is never closed or reused while the process runs.
var wakeStartupHoldFile *os.File

// carryWakeStartupHold passes this wake's startup hold to the successor a
// wake restart or self-upgrade execs, so a restart during the trust dialog
// keeps holding input. A hold coop exec already released reads EOF at once
// in the successor. It returns env unchanged when this wake has no hold.
func carryWakeStartupHold(env []string) []string {
	if wakeStartupHoldFile == nil {
		return env
	}
	fd := wakeStartupHoldFile.Fd()
	flags, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
	if err != nil {
		return env
	}
	if _, err := unix.FcntlInt(fd, unix.F_SETFD, flags&^unix.FD_CLOEXEC); err != nil {
		return env
	}
	return setEnvVar(unsetEnvVar(env, envWakeStartupHold), envWakeStartupHold, strconv.Itoa(int(fd)))
}

// withWakeStartupHold reads every dialog as open until released is closed.
func withWakeStartupHold(source func(time.Time) (wakeDialogState, error), released <-chan struct{}) func(time.Time) (wakeDialogState, error) {
	if released == nil {
		return source
	}
	return func(now time.Time) (wakeDialogState, error) {
		select {
		case <-released:
			return source(now)
		default:
			return wakeDialogOpen, nil
		}
	}
}

// inspectWakeStartupHold reports the agent's Startup marker while it holds
// input, or nil.
func inspectWakeStartupHold(root, handle string, now time.Time) *wakeStartupHoldStatus {
	path := filepath.Join(fsq.AgentBase(root, handle), wakeDialogFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	raw, err := fsq.ReadRegularNoFollowBounded(path, maxWakeMetadataFileBytes)
	var marker wakeDialogMarker
	// The wake's own decision: a marker whose trust is recorded holds nothing.
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent ||
		startupDialogClosed(raw, &startupDialogCache{}) {
		return nil
	}
	release := fmt.Sprintf("the doorbell rings once %s records that it trusts the directory, or when its SessionStart dialog hook runs; input stays held, with attention output after %s",
		marker.Harness, wakeDialogQuietHold)
	if marker.Harness == launch.CodexProvider {
		release += "; a directory kept untrusted releases only through the Codex hook: amq wake install-dialog-hook --harness codex"
	}
	return &wakeStartupHoldStatus{
		Harness: marker.Harness, Dir: marker.Cwd,
		AgeSeconds: int64(now.Sub(info.ModTime()).Seconds()), Release: release,
	}
}
