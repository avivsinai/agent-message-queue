//go:build darwin || linux

package cli

import (
	"context"
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
// it execs a harness that may show a startup prompt. A doorbell at the trust
// dialog ends the session: Claude Code's Enter picks the default "No, exit",
// and Codex quits. A doorbell at Codex's update prompt runs the upgrade. A
// trust marker closes when the harness records that it trusts the
// directory. Every Startup marker also closes when a harness dialog hook
// removes it: Claude Code's at SessionStart, Codex's at the first turn,
// since Codex runs SessionStart hooks only when a turn starts
// (core/src/session/turn.rs run_pending_session_start_hooks).
const wakeDialogStartupEvent = "Startup"

// wakeStartupKindUpdate marks a Codex Startup marker written only because
// the update prompt could not be ruled out (startupDialogMarkerFor). The
// dialog hook releases it, and so does the wake's startup recheck once
// Codex's own state rules the prompt out, when the marker keeps a probe
// plan.
const wakeStartupKindUpdate = "update_prompt"

// wakeStartupKindReview marks a Codex Startup marker for a release that
// reviews hooks after its trust screen (codexHookReviewSince): the
// directory trust is not recorded, or Codex's own hooks/list did not show
// that no hook needs review. A trust record does not show that the hook
// review closed, so the dialog hook releases it, or the wake's startup
// recheck once Codex's hooks/list shows no hook to review.
const wakeStartupKindReview = "hook_review"

// wakeStartupKindTermDumb marks a Codex Startup marker for a seat whose
// terminal Codex reads as dumb: Codex asks "Continue anyway? [y/N]" before
// its TUI starts (cli/src/main.rs:2518-2533 at rust-v0.164.0), and the
// doorbell's Enter answers No. Codex records no answer, so only the dialog
// hook releases it.
const wakeStartupKindTermDumb = "term_dumb"

// startupTrust is what the harness's own trust record says about a
// directory.
type startupTrust uint8

const (
	// startupTrustUnknown: the record cannot be read or does not decide.
	// coop exec writes no trust marker for it, and an open marker stays
	// open; a Codex release that reviews hooks, or one AMQ cannot verify,
	// still gets a marker (startupDialogMarkerFor).
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
	case launch.CursorProvider:
		for _, arg := range args {
			if arg == "--" {
				break
			}
			if arg == "-p" || arg == "--print" {
				return "", "", startupTrustUnknown
			}
		}
		// Trust can be followed by MCP or sandbox prompts. Only the
		// session-ready hook or a human submission releases this hold.
		return "cursor", wd, startupTrustPending
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
// tui/src/config_update.rs read_remote_project_trust): a [projects] entry
// for the directory or its git trust root with trust_level "trusted" asks
// nothing; no entry, or a saved "untrusted" one (a restricted dialog),
// asks. Outside a git checkout codex-cli 0.145 and 0.163 asked on every
// start in one folder and recorded nothing, and asked nothing in another;
// the decision there turns on project layers AMQ does not read, so trust
// there stays pending.
func codexStartupTrust(dir string) startupTrust {
	codexHome, err := codexHomeDir()
	if err != nil {
		if codexNearestGitAncestor(dir) == "" {
			return startupTrustPending
		}
		return startupTrustUnknown
	}
	return codexStartupTrustIn(codexHome, dir)
}

// codexStartupTrustIn is codexStartupTrust for the Codex home codexHome.
func codexStartupTrustIn(codexHome, dir string) startupTrust {
	if codexNearestGitAncestor(dir) == "" {
		return startupTrustPending
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

// syncStartupDialogMarker writes the Startup marker when the harness about
// to start as binaryPath with its final args may show a startup prompt, so
// the wake holds the doorbell until the prompt is answered: a trust dialog
// still pending, or for Codex an update prompt its state does not rule out.
// Otherwise it removes a Startup marker an earlier launch left (one
// declined, or one that never started), which would hold this session's
// doorbell. recorded reports that coop exec recorded the identity of the
// Codex thread args resume. The caller owns the agent's wake.
func syncStartupDialogMarker(root, handle, binaryPath string, args []string, recorded bool) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	marker, hold := startupDialogMarkerFor(binaryPath, args, wd, recorded)
	return syncStartupDialogMarkerState(root, handle, marker, hold)
}

// syncStartupDialogMarkerState publishes the decided hold through the
// caller's existing agent directory.
func syncStartupDialogMarkerState(root, handle string, marker wakeDialogMarker, hold bool) error {
	agentDir, err := openExistingWakeAgentDir(root, handle)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return agentDir.withFD(func(dirfd int) error {
		if !hold {
			return removeWakeDialogMarkerIf(dirfd, agentDir, func(current []byte) bool {
				var marker wakeDialogMarker
				return json.Unmarshal(current, &marker) == nil && marker.Event == wakeDialogStartupEvent
			})
		}
		marker.OpenedAt = time.Now().UTC().Format(time.RFC3339)
		raw, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		return replaceWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'))
	})
}

// startupDialogMarkerFor returns the Startup marker a harness started as
// binaryPath with args in wd needs, and whether it needs one.
func startupDialogMarkerFor(binaryPath string, args []string, wd string, recorded bool) (wakeDialogMarker, bool) {
	harness, dir, trust := startupTrustDialog(binaryPath, args, wd)
	if harness != launch.CodexProvider {
		return startupDialogMarkerDecision(harness, dir, trust, true)
	}
	installed, known := codexInstalledVersion(binaryPath)
	cacheRuledOut := codexUpdatePromptRuledOut(installed, known)
	// The verified releases before hook review keep the trust and version
	// cache decision. Their app-server answers config/read and
	// configRequirements/read too (app-server-protocol common.rs at
	// rust-v0.145.0, 0.161.0 and 0.163.0), so an update prompt marker keeps
	// the probe's plan for the wake's startup recheck.
	if known && codexUpdateVersionVerified(installed) && codexVersionNewer(codexHookReviewSince, installed) && !codexTerminalDumb() {
		marker, hold := startupDialogMarkerDecision(harness, dir, trust, cacheRuledOut)
		if hold && marker.Kind == wakeStartupKindUpdate {
			if physical, ok := codexPhysicalLaunchDir(args, wd); ok {
				ctx, cancel := context.WithTimeout(context.Background(), codexHooksProbeTimeout)
				plan, planned := planCodexStartupProbe(ctx, binaryPath, args, wd, physical, recorded)
				cancel()
				if planned {
					marker.Probe = withCodexVersion(plan, installed, known)
				}
			}
		}
		return marker, hold
	}
	// Any other release may review hooks after its trust screen. It starts
	// clear only when Codex records that it trusts the directory, its
	// hooks/list shows no hook to review, and the update prompt is ruled
	// out: by the version cache of a verified release, or by an effective
	// check_for_update_on_startup of false that no requirement pins on
	// (tui/src/updates.rs:28 and :153 at rust-v0.164.0: the prompt needs
	// that setting in effect). The marker keeps the probe's plan, so the
	// wake can repeat the probe and release on the same proof.
	marker := wakeDialogMarker{Schema: 1, Event: wakeDialogStartupEvent, Harness: harness, Cwd: dir, Kind: wakeStartupKindUpdate}
	if codexTerminalDumb() {
		marker.Kind = wakeStartupKindTermDumb
		return marker, true
	}
	// Codex asks hooks/list for its config.cwd, which starts from the
	// physical working directory, and project hooks are keyed by that
	// spelling; a directory that cannot be resolved proves nothing.
	physical, resolved := codexPhysicalLaunchDir(args, wd)
	if trust != startupTrustAccepted {
		if cacheRuledOut {
			marker.Kind = wakeStartupKindReview
		}
		if resolved {
			ctx, cancel := context.WithTimeout(context.Background(), codexHooksProbeTimeout)
			plan, ok := planCodexStartupProbe(ctx, binaryPath, args, wd, physical, recorded)
			cancel()
			if ok {
				marker.Probe = withCodexVersion(plan, installed, known)
			}
		}
		return marker, true
	}
	var probe codexStartupProbe
	if resolved {
		probe = probeCodexStartup(binaryPath, args, wd, physical, recorded)
	}
	if probe.plan != nil {
		marker.Probe = withCodexVersion(*probe.plan, installed, known)
	}
	if !cacheRuledOut && (!probe.updateCheckOff || !probe.requirementsClear) {
		return marker, true
	}
	if !probe.hooksClear {
		marker.Kind = wakeStartupKindReview
		return marker, true
	}
	return wakeDialogMarker{}, false
}

// withCodexVersion returns plan with the binary's version, when known.
func withCodexVersion(plan codexStartupPlan, installed [3]uint64, known bool) *codexStartupPlan {
	if known {
		plan.Version = fmt.Sprintf("%d.%d.%d", installed[0], installed[1], installed[2])
	}
	return &plan
}

// codexTerminalDumb reports whether Codex reads this terminal as dumb and
// so asks "Continue anyway? [y/N]" first: TERM_PROGRAM (other than tmux)
// names dumb, or none of the terminal variables Codex checks before TERM is
// set and TERM is dumb (terminal-detection/src/lib.rs:254-330 at
// rust-v0.164.0).
func codexTerminalDumb() bool {
	nonEmpty := func(key string) bool { return strings.TrimSpace(os.Getenv(key)) != "" }
	if program := strings.TrimSpace(os.Getenv("TERM_PROGRAM")); program != "" && !strings.EqualFold(program, "tmux") {
		return strings.EqualFold(program, "dumb")
	}
	if nonEmpty("GHOSTTY_RESOURCES_DIR") {
		return false
	}
	for _, key := range []string{"WEZTERM_VERSION", "ITERM_SESSION_ID", "ITERM_PROFILE", "ITERM_PROFILE_NAME", "KITTY_WINDOW_ID",
		"ALACRITTY_SOCKET", "KONSOLE_VERSION", "GNOME_TERMINAL_SCREEN", "VTE_VERSION", "WT_SESSION"} {
		if _, set := os.LookupEnv(key); set {
			return false
		}
	}
	return strings.TrimSpace(os.Getenv("TERM")) == "dumb"
}

// codexPhysicalLaunchDir is the cwd a Codex TUI started with args in wd
// sends to hooks/list, spelled as Codex spells it
// (tui/src/startup_orchestration.rs:278-282 and lib.rs:948-962
// config_cwd_for_app_server_target at rust-v0.164.0): without -C/--cd the
// process's physical working directory (wd with symlinks resolved); with
// one, its value joined to that directory and canonicalized as
// codexCanonicalizePreservingSymlinks does.
func codexPhysicalLaunchDir(args []string, wd string) (string, bool) {
	real, err := filepath.EvalSymlinks(wd)
	if err != nil {
		return "", false
	}
	return codexCanonicalizePreservingSymlinks(codexLaunchDir(args, real))
}

// codexCanonicalizePreservingSymlinks ports Codex's
// canonicalize_existing_preserving_symlinks
// (utils/absolute-path/src/lib.rs:210-227 at rust-v0.164.0): the realpath
// of logical, except that logical stays as spelled when the realpath
// differs and logical or an ancestor is a symlink that has a grandparent.
// A top-level symlink such as macOS /tmp therefore reads as its realpath.
// A path that cannot be resolved fails, as it does for Codex.
func codexCanonicalizePreservingSymlinks(logical string) (string, bool) {
	logical = filepath.Clean(logical)
	canonical, err := filepath.EvalSymlinks(logical)
	if err != nil {
		return "", false
	}
	if canonical == logical {
		return canonical, true
	}
	for ancestor := logical; ; ancestor = filepath.Dir(ancestor) {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return canonical, true
		}
		if info, err := os.Lstat(ancestor); err == nil && info.Mode()&os.ModeSymlink != 0 && filepath.Dir(parent) != parent {
			return logical, true
		}
	}
}

// startupDialogMarkerDecision keeps trust and update evidence independent.
// A trust decision cannot release a possible update prompt.
func startupDialogMarkerDecision(harness, dir string, trust startupTrust, updateRuledOut bool) (wakeDialogMarker, bool) {
	marker := wakeDialogMarker{Schema: 1, Event: wakeDialogStartupEvent, Harness: harness, Cwd: dir}
	if harness == "" {
		return wakeDialogMarker{}, false
	}
	// A trust record releases a trust marker; a second seat's Codex can
	// record trust for the same root while this one still shows its update
	// prompt, so a possible update prompt takes the kind only the hook
	// releases.
	if harness == launch.CodexProvider && !updateRuledOut {
		marker.Kind = wakeStartupKindUpdate
		return marker, true
	}
	return marker, trust == startupTrustPending
}

// startupDialogClosed reports whether marker is a Startup trust marker whose
// harness now records that it trusts the directory. A record that cannot be
// read or does not decide keeps the marker open, and so does an update
// prompt marker. cache keeps the last answer while the harness's trust file
// is unchanged, so a held wake does not reparse it on every recheck.
func startupDialogClosed(raw []byte, cache *startupDialogCache) bool {
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent || marker.Kind != "" ||
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
// holds input (startupDialogClosed, as the wake decides). Without a Codex
// record of the answer the hold releases only through the Codex dialog
// hook at the session's first turn; while it holds, the wake shows the
// held-doorbell notice.
func codexStartupHoldHint(root, handle string) (opsHint, bool) {
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(fsq.AgentBase(root, handle), wakeDialogFileName), maxWakeMetadataFileBytes)
	var marker wakeDialogMarker
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent || marker.Harness != launch.CodexProvider ||
		startupDialogClosed(raw, &startupDialogCache{}) {
		return opsHint{}, false
	}
	return opsHint{
		Code:    "codex_trust_hold",
		Status:  "warn",
		Message: fmt.Sprintf("%s's doorbell is held: %s", handle, codexStartupHoldRelease(marker)),
	}, true
}

// codexStartupHookRelease says how the Codex dialog hook releases a
// Startup hold, with the remedy when it cannot run: Codex skips an
// installed hook it does not trust, so the remedy then is the trust, not
// another install.
func codexStartupHookRelease() string {
	check, ok := checkCodexDialogHook()
	switch {
	case ok && check.Status == "ok":
		return "the Codex dialog hook releases it at the first prompt typed in that Codex session"
	case ok && check.Message == codexDialogHookUntrusted:
		return "the Codex dialog hook releases it at the first prompt typed in that Codex session once Codex trusts the hook (open /hooks in Codex and trust the AMQ_DIALOG_HOOK entries)"
	}
	return "the Codex dialog hook releases it at the first prompt typed in that Codex session once it runs (amq wake install-dialog-hook --harness codex, then trust the AMQ_DIALOG_HOOK entries in Codex /hooks)"
}

// codexStartupHoldRemedy is the fixed held-notice text that says what
// releases a Codex Startup hold once its screen is answered. probed
// reports a marker the wake's trust read or startup recheck can release.
// It says that a typed prompt releases it only when the dialog hook can
// run, and names the remedy otherwise.
func codexStartupHoldRemedy(probed bool) string {
	state := ""
	if probed {
		state = "the doorbell rings once Codex's state shows the screen closed; "
	}
	check, ok := checkCodexDialogHook()
	switch {
	case ok && check.Status == "ok":
		return state + "a prompt typed there releases it too"
	case ok && check.Message == codexDialogHookUntrusted:
		return state + "a prompt typed there releases it once the AMQ_DIALOG_HOOK entries are trusted in Codex /hooks"
	}
	return state + "a prompt typed there releases it once amq wake install-dialog-hook --harness codex runs and Codex /hooks trusts the entries"
}

// codexStartupHoldRelease says why a Codex Startup marker holds the
// doorbell, what releases it, and the remedy.
func codexStartupHoldRelease(marker wakeDialogMarker) string {
	hook := codexStartupHookRelease()
	if marker.Probe != nil && marker.Kind != wakeStartupKindTermDumb {
		hook = "the wake rechecks Codex's state every 10 s and rings once Codex records the trust, no hook needs review, and the update prompt is ruled out; or " + hook
	}
	switch {
	case marker.Kind == wakeStartupKindTermDumb:
		return "Codex reads this terminal as dumb (TERM=dumb) and asks \"Continue anyway? [y/N]\", where the doorbell's Enter answers No; " + hook
	case marker.Kind == wakeStartupKindReview:
		return fmt.Sprintf("Codex may ask whether to trust %s or to review new or changed hooks, and a trust record does not show that the hook review closed; %s", marker.Cwd, hook)
	case marker.Kind == wakeStartupKindUpdate:
		return "Codex may show its update prompt, whose default choice runs the upgrade; " + hook +
			"; setting " + codexUpdateCheckKey + " = false in $CODEX_HOME/config.toml (default ~/.codex/config.toml) prevents ordinary update prompts; it does not release this open hold, and a later launch starts without one only when Codex reports the check off and no requirement turns it on"
	case codexNearestGitAncestor(marker.Cwd) == "":
		return fmt.Sprintf("Codex may ask whether to trust %s, which is outside a git checkout, where AMQ cannot read Codex's decision; %s", marker.Cwd, hook)
	}
	return fmt.Sprintf("Codex asks whether to trust %s; it rings once Codex records trust, or %s", marker.Cwd, hook)
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
	const held = "input stays held, and the wake shows the held-doorbell notice at once"
	release := fmt.Sprintf("the doorbell rings once %s records that it trusts the directory, or when its SessionStart dialog hook runs; %s",
		marker.Harness, held)
	if marker.Harness == launch.CodexProvider {
		release = codexStartupHoldRelease(marker) + "; " + held
	}
	return &wakeStartupHoldStatus{
		Harness: marker.Harness, Dir: marker.Cwd,
		AgeSeconds: int64(now.Sub(info.ModTime()).Seconds()), Release: release,
	}
}
