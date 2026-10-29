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
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"golang.org/x/sys/unix"
)

// wakeDialogStartupEvent marks a .wake.dialog that coop exec writes before
// it execs a harness that may show a startup prompt, only when the person
// installed the harness's AMQ dialog hook (dialogGuardEnabled): the hook is
// what releases it. Without the hook coop exec writes none, and the
// doorbell rings plain from the start. A doorbell at the trust
// dialog ends the session: Claude Code's Enter picks the default "No, exit",
// and Codex quits. A doorbell at Codex's update prompt runs the upgrade. A
// trust marker closes when the harness records that it trusts the
// directory. Every Startup marker also closes when a harness dialog hook
// removes it: Claude Code's at SessionStart, Codex's at the first turn,
// since Codex runs SessionStart hooks only when a turn starts
// (core/src/session/turn.rs run_pending_session_start_hooks).
const wakeDialogStartupEvent = "Startup"

// wakeStartupKindUpdate marks a Codex Startup marker written only because
// the update prompt could not be ruled out (startupDialogMarkerFor). No
// Codex record shows that prompt closed, so only the dialog hook releases
// it.
const wakeStartupKindUpdate = "update_prompt"

// wakeStartupKindReview marks a Codex Startup marker for a release that
// reviews hooks after its trust screen (codexHookReviewSince): the
// directory trust is not recorded, or Codex's own hooks/list did not show
// that no hook needs review. A trust record does not show that the hook
// review closed, so only the dialog hook releases it.
const wakeStartupKindReview = "hook_review"

// wakeStartupKindResumeDir marks a Codex Startup marker for a resume or
// fork that does not resume in the launch directory: with tui.resume_cwd
// "session" Codex resumes in the thread's own directory, whose trust and
// hooks AMQ does not check, and a mode AMQ cannot read may ask which
// directory to use. Only the dialog hook releases it.
const wakeStartupKindResumeDir = "resume_dir"

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
	if codexNearestGitAncestor(dir) == "" {
		return startupTrustPending
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

// syncStartupDialogMarker writes the Startup marker when the harness about
// to start as binaryPath with its final args has its dialog guard on and
// may show a startup prompt, so the wake holds the doorbell until the
// prompt is answered: a trust dialog still pending, or for Codex an update
// prompt its state does not rule out. Otherwise it removes a Startup marker
// an earlier launch left (one declined, or one that never started), which
// would hold this session's doorbell. recorded reports that coop exec
// recorded the identity of the Codex thread args resume. The caller owns
// the agent's wake.
func syncStartupDialogMarker(root, handle, binaryPath string, args []string, recorded bool) error {
	marker, hold, err := decideStartupDialogMarker(binaryPath, args, recorded)
	if err != nil {
		return err
	}
	return publishStartupDialogMarker(root, handle, marker, hold, launchDialogGuard(binaryPath))
}

// launchDialogGuard is the harness whose AMQ dialog hook guards a launch of
// binaryPath (claude, codex or cursor), or "" for a plain doorbell.
func launchDialogGuard(binaryPath string) string {
	facts, _ := harness.ForExecutable(binaryPath)
	switch facts.Name {
	case harness.Claude, harness.Codex, harness.Cursor:
		if dialogGuardEnabled(facts.Name) {
			return facts.Name
		}
	}
	return ""
}

// codexLaunchDecision is a Codex seat's startup marker and backend for one
// launch.
type codexLaunchDecision struct {
	marker        wakeDialogMarker
	hold, decided bool
	// embeddedOnly keeps a seat that could run on the Codex daemon on its
	// own app-server for this launch: no daemon thread and no thread record.
	embeddedOnly bool
}

// decideCodexLaunch decides a Codex seat's startup marker before naming and
// recording. A seat on the daemon runs its hooks in the daemon, where no
// prompt hook proves this launch left its startup screens. So a seat whose
// screen may show runs its own app-server for this launch, with the
// identity overrides, no daemon thread and no thread record, and its own
// children run the dialog hook (startupPromptOfLaunch). A seat stays on the
// daemon only when no screen can show. Without the Codex dialog guard
// nothing is decided: the seat stays on the daemon and gets no marker.
func decideCodexLaunch(binaryPath string, args, env []string, wd string, named coopNamedChoice, resumeChoice string) (codexLaunchDecision, error) {
	var decision codexLaunchDecision
	var err error
	if !dialogGuardEnabled(harness.Codex) {
		return decision, nil
	}
	_, selected, unresolved := codexResumeSelection(args)
	daemonCandidate := selected && !unresolved ||
		named.enabled && coopNamedModeFor(binaryPath) == coopNamedModeTUI && !agentArgsPreventAutoNameFor(binaryPath, args)
	if daemonCandidate {
		daemonArgs := pinCodexResumeLaunchDir(binaryPath, args, wd, true, resumeChoice)
		if decision.marker, decision.hold, err = decideStartupDialogMarker(binaryPath, daemonArgs, true); err != nil {
			return codexLaunchDecision{}, err
		}
		if !decision.hold {
			decision.decided = true
			return decision, nil
		}
	}
	embeddedArgs := pinCodexShellIdentity(binaryPath, pinCodexResumeLaunchDir(binaryPath, args, wd, false, resumeChoice), env, false)
	if decision.marker, decision.hold, err = decideStartupDialogMarker(binaryPath, embeddedArgs, false); err != nil {
		return codexLaunchDecision{}, err
	}
	decision.decided, decision.embeddedOnly = true, daemonCandidate
	return decision, nil
}

// decideStartupDialogMarker is the Startup marker decision of
// syncStartupDialogMarker, without publishing it.
func decideStartupDialogMarker(binaryPath string, args []string, recorded bool) (wakeDialogMarker, bool, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wakeDialogMarker{}, false, err
	}
	marker, hold := startupDialogMarkerFor(binaryPath, args, wd, recorded)
	if hold && marker.Harness == launch.CodexProvider {
		// coop exec execs Codex in this process, so its pid and start time
		// name the launch whose prompt hook may release the marker.
		if holder, err := launch.HolderOf(os.Getpid()); err == nil {
			marker.Launch = &holder
		}
	}
	return marker, hold, nil
}

// publishStartupDialogMarker publishes a decided Startup marker and says on
// stderr which doorbell the launch gets: "dialog guard: plain" (text and
// Enter, never held for a dialog), or "dialog guard: proper (<harness>)"
// when guard names the harness whose dialog hook or relay holds it.
func publishStartupDialogMarker(root, handle string, marker wakeDialogMarker, hold bool, guard string) error {
	if err := syncStartupDialogMarkerState(root, handle, marker, hold); err != nil {
		return err
	}
	if guard == "" {
		_ = writeStderr("dialog guard: plain\n")
	} else {
		_ = writeStderr("dialog guard: proper (%s)\n", guard)
	}
	return nil
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
// Only a harness whose dialog guard is on gets one: its hook is what
// releases it.
func startupDialogMarkerFor(binaryPath string, args []string, wd string, recorded bool) (wakeDialogMarker, bool) {
	harness, dir, trust := startupTrustDialog(binaryPath, args, wd)
	if harness == "" || !dialogGuardEnabled(harness) {
		return wakeDialogMarker{}, false
	}
	if harness != launch.CodexProvider {
		return startupDialogMarkerDecision(harness, dir, trust, true)
	}
	// The trust and hooks evidence below is for the launch directory. A
	// resume in the thread's own directory ("session") has none, a mode AMQ
	// cannot read may still ask which directory to use, and arguments AMQ
	// cannot parse may be a resume or fork.
	if at, known := codexResumeOrFork(args); !known || at >= 0 && codexEffectiveResumeCwd(binaryPath, args, wd, recorded) != "current" {
		return wakeDialogMarker{Schema: 1, Event: wakeDialogStartupEvent, Harness: harness, Cwd: dir, Kind: wakeStartupKindResumeDir}, true
	}
	installed, known := codexInstalledVersion(binaryPath)
	cacheRuledOut := codexUpdatePromptRuledOut(installed, known)
	// The verified releases before hook review keep the trust and version
	// cache decision.
	if known && codexUpdateVersionVerified(installed) && codexVersionNewer(codexHookReviewSince, installed) {
		return startupDialogMarkerDecision(harness, dir, trust, cacheRuledOut)
	}
	// Any other release may review hooks after its trust screen. It starts
	// clear only when Codex records that it trusts the directory, its
	// hooks/list shows no hook to review, and the update prompt is ruled
	// out: by the version cache of a verified release, or by an effective
	// check_for_update_on_startup of false that no requirement pins on
	// (tui/src/updates.rs:28 and :153 at rust-v0.164.0: the prompt needs
	// that setting in effect).
	marker := wakeDialogMarker{Schema: 1, Event: wakeDialogStartupEvent, Harness: harness, Cwd: dir, Kind: wakeStartupKindUpdate}
	if trust != startupTrustAccepted {
		if cacheRuledOut {
			marker.Kind = wakeStartupKindReview
		}
		return marker, true
	}
	// Codex asks hooks/list for its config.cwd, which starts from the
	// physical working directory, and project hooks are keyed by that
	// spelling; a directory that cannot be resolved proves nothing.
	var probe codexStartupProbe
	if physical, ok := codexPhysicalLaunchDir(args, wd); ok {
		probe = probeCodexStartup(binaryPath, args, wd, physical, recorded)
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

// pinCodexResumeLaunchDir keeps a Codex resume or fork launch in its
// launch directory without the directory prompt, when choice (the
// effective tui.resume_cwd, codexEffectiveResumeCwd) is proven unset.
// Codex asks which directory to resume in only when the resume mode is
// unset and the thread's recorded cwd differs from the launch directory
// (resolve_cwd_for_resume_or_fork, tui/src/session_resume.rs:84-141 at
// rust-v0.164.0), and Enter picks a directory. A mode of current skips the
// prompt and keeps the launch directory (session_resume.rs:35-43;
// tui.resume_cwd, config/src/types.rs:114-122 and 995-998). A seat that
// runs its own app-server gets -c tui.resume_cwd="current", as it gets
// check_for_update_on_startup=false. A recorded seat stays on the Codex
// daemon, where any -c override other than a feature boolean would make
// its TUI run its own app-server (config_exclusion,
// tui/src/daemon_startup.rs:89-113), so it gets -C <wd>, which makes the
// mode current too. A set or unknown choice keeps the args.
func pinCodexResumeLaunchDir(binaryPath string, args []string, wd string, recorded bool, choice string) []string {
	if at, known := codexResumeOrFork(args); launch.ProviderForExecutable(binaryPath) != launch.CodexProvider ||
		codexArgsHaveRemote(args) || !known || at < 0 || choice != "" {
		return args
	}
	if recorded {
		return append([]string{"-C", wd}, args...)
	}
	return append([]string{"-c", `tui.resume_cwd="current"`}, args...)
}

// codexEffectiveResumeCwd is the resume directory mode a Codex launch runs
// with: "current" for -C/--cd, and otherwise the effective tui.resume_cwd
// from the launch-time probe's config/read ("" when unset). That probe runs
// with the seat's whole -c, --enable and --disable sequence, which Codex
// applies in order, the last one winning (config/src/overrides.rs:9-13 and
// 31-65 at rust-v0.164.0), and merges system, user and trusted project
// layers. A probe that cannot run or answer, as for a --profile seat,
// gives "unknown".
func codexEffectiveResumeCwd(binaryPath string, args []string, wd string, recorded bool) string {
	if codexArgsHaveCd(args) {
		return "current"
	}
	physical, ok := codexPhysicalLaunchDir(args, wd)
	if !ok {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexHooksProbeTimeout)
	defer cancel()
	plan, ok := planCodexStartupProbe(ctx, binaryPath, args, wd, physical, recorded)
	if !ok {
		return "unknown"
	}
	probe := runCodexStartupProbe(ctx, plan)
	if !probe.configRead {
		return "unknown"
	}
	return probe.resumeCwd
}

// codexArgsHaveCd reports a -C/--cd before the prompt, which makes Codex's
// resume mode current whatever the configuration says
// (effective_resume_cwd_mode, tui/src/session_resume.rs:35-43).
func codexArgsHaveCd(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if flag, _, _ := strings.Cut(arg, "="); flag == "-C" || flag == "--cd" {
			return true
		}
	}
	return false
}

// codexNonInteractiveSubcommands are the Codex subcommands that run no
// interactive TUI, so they never ask "Continue anyway?" in a dumb terminal
// (cli/src/main.rs Subcommand at rust-v0.164.0; only the bare TUI, agents,
// resume and fork reach run_interactive_tui). cloud runs its own TUI and is
// not listed.
var codexNonInteractiveSubcommands = map[string]bool{
	"exec": true, "e": true, "review": true, "login": true, "logout": true, "mcp": true,
	"plugin": true, "app-server": true, "remote-control": true, "app": true, "completion": true,
	"update": true, "doctor": true, "sandbox": true, "debug": true, "execpolicy": true,
	"apply": true, "a": true, "queue": true, "archive": true, "delete": true, "unarchive": true,
	"migrate-rollouts": true, "migrate-nfs-db": true, "responses-api-proxy": true,
	"stdio-to-uds": true, "exec-server": true, "features": true, "tcp-tunnel": true, "help": true,
}

// codexArgsNonInteractive reports a Codex launch AMQ positively identifies
// as running no interactive TUI: help or version, or a subcommand in
// codexNonInteractiveSubcommands after options AMQ knows. An unknown option
// (for example -i) or a prompt leaves it interactive.
func codexArgsNonInteractive(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return false
		case arg == "-h" || arg == "--help" || arg == "-V" || arg == "--version":
			return true
		case !strings.HasPrefix(arg, "-"):
			return codexNonInteractiveSubcommands[arg]
		}
		flag, _, inline := strings.Cut(arg, "=")
		switch {
		case (codexFlagOptions[flag] || codexEmbeddedFlags[flag]) && !inline:
		case codexValueOptions[flag]:
			if !inline {
				i++
			}
		default:
			return false
		}
	}
	return false
}

// codexResumeOrFork reads a Codex launch's grammar up to its first
// positional: at is the index of a resume or fork subcommand, or -1. known
// is false for arguments AMQ cannot classify (an option it does not know),
// which may still be a resume or fork. -i/--image takes one or more values,
// up to the next option, as Codex's num_args(1..) does
// (utils/cli/src/shared_options.rs at rust-v0.164.0).
func codexResumeOrFork(args []string) (at int, known bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return -1, true
		}
		if !strings.HasPrefix(arg, "-") {
			if arg == "resume" || arg == "fork" {
				return i, true
			}
			return -1, true
		}
		flag, _, inline := strings.Cut(arg, "=")
		switch {
		case flag == "-i" || flag == "--image":
			if !inline && (i+1 >= len(args) || strings.HasPrefix(args[i+1], "-")) {
				return -1, false
			}
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		case (codexFlagOptions[flag] || codexEmbeddedFlags[flag]) && !inline:
		case codexValueOptions[flag]:
			if !inline {
				i++
			}
		default:
			return -1, false
		}
	}
	return -1, true
}

// codexResumeOrForkAt is the index of a Codex launch's resume or fork
// subcommand, or -1 when it is not one or AMQ cannot tell.
func codexResumeOrForkAt(args []string) int {
	at, _ := codexResumeOrFork(args)
	return at
}

// codexTerminalDumbIn reports whether Codex reads the terminal of the
// environment lookup reads as dumb and so asks "Continue anyway? [y/N]"
// before its TUI starts (cli/src/main.rs:2518-2533 at rust-v0.164.0), where
// the doorbell's Enter answers No and no record shows the answer:
// TERM_PROGRAM (other than tmux) names dumb, or none of the terminal
// variables Codex checks before TERM is set and TERM is dumb
// (terminal-detection/src/lib.rs:254-330 at rust-v0.164.0).
func codexTerminalDumbIn(lookup func(string) (string, bool)) bool {
	get := func(key string) string { value, _ := lookup(key); return strings.TrimSpace(value) }
	if program := get("TERM_PROGRAM"); program != "" && !strings.EqualFold(program, "tmux") {
		return strings.EqualFold(program, "dumb")
	}
	if get("GHOSTTY_RESOURCES_DIR") != "" {
		return false
	}
	for _, key := range codexTerminalDetectionKeys {
		if _, set := lookup(key); set {
			return false
		}
	}
	return get("TERM") == "dumb"
}

// codexTerminalDetectionKeys are the variables whose presence Codex reads
// as a known terminal before it looks at TERM.
var codexTerminalDetectionKeys = []string{"WEZTERM_VERSION", "ITERM_SESSION_ID", "ITERM_PROFILE", "ITERM_PROFILE_NAME", "KITTY_WINDOW_ID",
	"ALACRITTY_SOCKET", "KONSOLE_VERSION", "GNOME_TERMINAL_SCREEN", "VTE_VERSION", "WT_SESSION"}

// codexDumbTerminalRefusal refuses a Codex launch with args whose terminal,
// read through lookup, Codex reads as dumb, unless the launch runs no
// interactive TUI (codexArgsNonInteractive).
func codexDumbTerminalRefusal(args []string, lookup func(string) (string, bool)) error {
	if codexArgsNonInteractive(args) || !codexTerminalDumbIn(lookup) {
		return nil
	}
	return UsageError("interactive Codex reads this terminal as dumb (TERM=dumb) and asks \"Continue anyway? [y/N]\" first, which a doorbell would answer; set TERM to your terminal's type (for example TERM=xterm-256color) and run again")
}

// managedCodexDumbTerminalRefusal applies codexDumbTerminalRefusal to a
// managed launch: the ticket's provider and target arguments, with its
// environment overlay over this process's environment.
func managedCodexDumbTerminalRefusal(ticket *launch.ExecutionTicket) error {
	if ticket == nil || ticket.Provider != launch.CodexProvider {
		return nil
	}
	index := 0
	if ticket.Wrapper != nil {
		index = len(ticket.Wrapper.Args) + 1
	}
	var args []string
	if index < len(ticket.TargetArgv) {
		args = ticket.TargetArgv[index+1:]
	}
	lookup := func(key string) (string, bool) {
		if value, ok := ticket.TargetEnv[key]; ok {
			return value, true
		}
		return os.LookupEnv(key)
	}
	return codexDumbTerminalRefusal(args, lookup)
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
	// A possible Codex update prompt takes its own kind, for the notice;
	// every Codex marker releases only through its launch's prompt hook.
	if harness == launch.CodexProvider && !updateRuledOut {
		marker.Kind = wakeStartupKindUpdate
		return marker, true
	}
	return marker, trust == startupTrustPending
}

// startupDialogClosed reports whether marker is a Claude Code Startup trust
// marker whose harness now records that it trusts the directory. A Codex
// Startup marker never closes on a record: the trust record is shared by
// every terminal, and another terminal's answer does not close this one's
// screen, so only a prompt hook of its launch releases it. A record that cannot be
// read or does not decide keeps the marker open, and so does an update
// prompt marker. cache keeps the last answer while the harness's trust file
// is unchanged, so a held wake does not reparse it on every recheck.
func startupDialogClosed(raw []byte, cache *startupDialogCache) bool {
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent || marker.Kind != "" ||
		marker.Harness != launch.ClaudeProvider || !filepath.IsAbs(marker.Cwd) {
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
// holds input (wakeStartupMarkerHolds, as the wake decides). Without a
// Codex record of the answer the hold releases only through the Codex
// dialog hook at the session's first turn; while it holds, the wake shows
// the held-doorbell notice.
func codexStartupHoldHint(root, handle string) (opsHint, bool) {
	path := filepath.Join(fsq.AgentBase(root, handle), wakeDialogFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return opsHint{}, false
	}
	raw, err := fsq.ReadRegularNoFollowBounded(path, maxWakeMetadataFileBytes)
	var marker wakeDialogMarker
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent || marker.Harness != launch.CodexProvider ||
		!wakeStartupMarkerHolds(raw, info.ModTime(), time.Now()) {
		return opsHint{}, false
	}
	return opsHint{
		Code:    "codex_trust_hold",
		Status:  "warn",
		Message: fmt.Sprintf("%s's doorbell is held: %s", handle, codexStartupHoldRelease(marker)),
	}, true
}

// codexStartupHookRelease says how the Codex dialog hook releases a
// Startup hold. coop exec writes the marker only while that hook is
// installed and trusted.
const codexStartupHookRelease = "the Codex dialog hook releases it at the first prompt typed in that Codex session"

// codexStartupHoldRemedy is the fixed held-notice text that says what
// releases a Codex Startup hold once its screen is answered.
const codexStartupHoldRemedy = "a prompt typed there releases it"

// codexStartupHoldRelease says why a Codex Startup marker holds the
// doorbell, what releases it, and the remedy.
func codexStartupHoldRelease(marker wakeDialogMarker) string {
	hook := codexStartupHookRelease
	switch {
	case marker.Kind == wakeStartupKindResumeDir:
		return "Codex resumes the thread in its own directory or may ask which directory to use (tui.resume_cwd is session or cannot be read), where a trust, hook review or directory screen can show; " + hook
	case marker.Kind == wakeStartupKindReview:
		return fmt.Sprintf("Codex may ask whether to trust %s or to review new or changed hooks, and a trust record does not show that the hook review closed; %s", marker.Cwd, hook)
	case marker.Kind == wakeStartupKindUpdate:
		return "Codex may show its update prompt, whose default choice runs the upgrade; " + hook +
			"; setting " + codexUpdateCheckKey + " = false in $CODEX_HOME/config.toml (default ~/.codex/config.toml) prevents ordinary update prompts; it does not release this open hold, and a later launch starts without one only when Codex reports the check off and no requirement turns it on"
	case codexNearestGitAncestor(marker.Cwd) == "":
		return fmt.Sprintf("Codex may ask whether to trust %s, which is outside a git checkout, where AMQ cannot read Codex's decision; %s", marker.Cwd, hook)
	}
	return fmt.Sprintf("Codex asks whether to trust %s; %s", marker.Cwd, hook)
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
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Event != wakeDialogStartupEvent ||
		!wakeStartupMarkerHolds(raw, info.ModTime(), now) {
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

// wakeStartupMarkerHolds reports whether a Startup marker read as raw with
// modTime holds input now, as the wake decides (wakeDialogSourceInDir): not
// when its harness recorded the trust that closes it, and not once it no
// longer proves an open dialog unless its harness's dialog guard is on.
func wakeStartupMarkerHolds(raw []byte, modTime, now time.Time) bool {
	if startupDialogClosed(raw, &startupDialogCache{}) {
		return false
	}
	state, _ := classifyWakeDialogMarker(modTime, true, nil, now)
	return state == wakeDialogOpen || dialogGuardEnabled(wakeDialogMarkerHarness(raw))
}
