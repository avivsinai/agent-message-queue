//go:build darwin || linux

package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const wakeReadyTimeout = 2 * time.Second

func runCoopExec(args []string) error {
	// Split at "--" before flag parsing so agent flags aren't consumed.
	amqArgs, agentArgs := splitDashDash(args)

	fs := flag.NewFlagSet("coop exec", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "Root directory (override auto-detection)")
	sessionFlag := fs.String("session", "", "Session name (shorthand for --root .agent-mail/<name>)")
	meFlag := fs.String("me", "", "Agent handle (override auto-derivation from command name)")
	noInitFlag := fs.Bool("no-init", false, "Don't auto-initialize if .amqrc is missing")
	noGitignoreFlag := fs.Bool("no-gitignore", false, "When auto-initializing, do not modify .gitignore")
	noWakeFlag := fs.Bool("no-wake", false, "Don't start amq wake in background")
	requireWakeFlag := fs.Bool("require-wake", false, "Fail if amq wake cannot start and acquire its lock")
	wakeInjectModeFlag := fs.String("wake-inject-mode", wakeInjectModeAuto, "Wake injection mode: auto, raw, paste, none")
	wakeInjectViaFlag := fs.String("wake-inject-via", "", "Start wake with this absolute --inject-via executable, enabling later amq wake repair")
	var wakeInjectArgFlags multiStringFlag
	fs.Var(&wakeInjectArgFlags, "wake-inject-arg", "Fixed argument for wake --inject-via before the payload (repeatable)")
	yesFlag := fs.Bool("y", false, "Skip confirmation prompts")

	usage := usageWithFlags(fs, "amq coop exec [options] <command> [-- <command-flags>]",
		"Set up co-op mode and exec into the agent (replaces this process).",
		"",
		"Sets AM_ROOT (always a session subdirectory) and AM_ME,",
		"starts amq wake in background, then",
		"replaces itself with the given command via exec.",
		"",
		"If neither --session nor --root is given, defaults to --session collab.",
		"The agent handle is derived from the command basename unless --me is set.",
		"",
		"Examples:",
		"  amq coop exec claude                              # Exec into Claude Code (session=collab)",
		"  amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Codex with flags",
		"  amq coop exec grok                                # Grok CLI, caller flags forwarded as-is",
		"  amq coop exec --session feature-x claude          # Isolated session",
		"  amq coop exec --root .agent-mail/auth claude      # Explicit root (no session default)",
		"  amq coop exec --require-wake --wake-inject-mode none claude  # Zero-input wake",
		"  amq coop exec --wake-inject-via /path/to/injector codex",
		"  amq coop exec --me myagent bash                   # Debug shell with AMQ env",
		"",
		"Zero-input readiness:",
		"  --wake-inject-mode none never reuses an existing wake unless its lock",
		"  proves it was also started in none mode; stop an older wake and retry.",
		"  A default request remains transport-agnostic and may reuse a none wake.",
	)

	if handled, err := parseFlags(fs, amqArgs, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *noWakeFlag && *requireWakeFlag {
		return UsageError("--require-wake cannot be used with --no-wake")
	}
	wakeInjectVia := strings.TrimSpace(*wakeInjectViaFlag)
	wakeInjectMode, err := normalizeWakeInjectMode(*wakeInjectModeFlag)
	if err != nil {
		return UsageError("--wake-inject-mode: %v", err)
	}
	if *wakeInjectViaFlag != "" && wakeInjectVia == "" {
		return UsageError("--wake-inject-via must not be blank")
	}
	if wakeInjectMode == wakeInjectModeNone && wakeInjectVia != "" {
		return UsageError("--wake-inject-via cannot be used with --wake-inject-mode none")
	}
	if wakeInjectMode == wakeInjectModeNone && len(wakeInjectArgFlags) > 0 {
		return UsageError("--wake-inject-arg cannot be used with --wake-inject-mode none")
	}
	if wakeInjectVia == "" && len(wakeInjectArgFlags) > 0 {
		return UsageError("--wake-inject-arg requires --wake-inject-via")
	}
	if wakeInjectVia != "" {
		resolvedWakeInjectVia, err := validateWakeInjectViaPath(wakeInjectVia)
		if err != nil {
			return UsageError("--wake-inject-via: %v", err)
		}
		wakeInjectVia = resolvedWakeInjectVia
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return UsageError("command required (e.g., 'claude', 'codex', 'bash')")
	}
	cmdName := remaining[0]
	// Extra positional args before "--" are appended to agent args.
	if len(remaining) > 1 {
		agentArgs = append(remaining[1:], agentArgs...)
	}

	// Derive agent handle from command basename (or --me override).
	agentHandle := *meFlag
	if agentHandle == "" {
		agentHandle = strings.ToLower(filepath.Base(cmdName))
	}
	agentHandle, err = normalizeHandle(agentHandle)
	if err != nil {
		return fmt.Errorf("cannot derive agent handle from %q: %w (use --me to override)", cmdName, err)
	}

	// Resolve explicit --session (pure sugar for --root <base>/<session>).
	if *sessionFlag != "" {
		if *rootFlag != "" {
			return UsageError("--session and --root are mutually exclusive")
		}
		if err := validateSessionName(*sessionFlag); err != nil {
			return err
		}
		base := resolveBaseRoot()
		*rootFlag = filepath.Join(base, *sessionFlag)
	}

	// Resolve root: --root flag (or --session-derived) > .amqrc > default.
	root := *rootFlag
	if root == "" {
		existing, existingErr := findAndLoadAmqrc()
		switch existingErr {
		case nil:
			root = existing.Config.Root
			if root != "" && !filepath.IsAbs(root) {
				root = filepath.Join(existing.Dir, root)
			}
		case errAmqrcNotFound:
			// Will auto-init below.
		default:
			return fmt.Errorf("invalid .amqrc: %w", existingErr)
		}
	}

	// Auto-init if needed (before session defaulting so full init fires on fresh projects).
	if root == "" || !dirExists(root) {
		if *noInitFlag {
			if root == "" {
				return fmt.Errorf("no .amqrc found and no --root specified; run 'amq coop init' first or remove --no-init")
			}
			return fmt.Errorf("root %q does not exist; run 'amq coop init' first or remove --no-init", root)
		}

		if root != "" {
			// We have a root (from --root, --session, or .amqrc) — create root + agent dirs.
			if err := fsq.EnsureRootDirs(root); err != nil {
				return fmt.Errorf("failed to create root %q: %w", root, err)
			}
			if err := fsq.EnsureAgentDirs(root, agentHandle); err != nil {
				return fmt.Errorf("failed to create mailbox for %s at %q: %w", agentHandle, root, err)
			}
		} else {
			// No --root flag and no .amqrc found: run full coop init (writes .amqrc).
			if !*yesFlag {
				ok, err := confirmPromptYes("No .amqrc found. Initialize co-op mode in current directory?")
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("initialization cancelled")
				}
			}

			var initArgs []string
			if *noGitignoreFlag {
				initArgs = []string{"--no-gitignore"}
			}
			if err := runCoopInitInternal(initArgs, false); err != nil {
				return fmt.Errorf("init failed: %w", err)
			}

			// Reload root after init.
			existing, existingErr := findAndLoadAmqrc()
			if existingErr != nil {
				return fmt.Errorf("failed to load .amqrc after init: %w", existingErr)
			}
			root = existing.Config.Root
			if root != "" && !filepath.IsAbs(root) {
				root = filepath.Join(existing.Dir, root)
			}
		}
	}

	// Default to --session collab when neither --session nor --root was specified.
	// This runs after auto-init so .amqrc exists and resolveBaseRoot() works.
	if *sessionFlag == "" && *rootFlag == "" {
		base := root // root is the literal .amqrc root (e.g., .agent-mail)
		root = filepath.Join(base, defaultSessionName)
		// Ensure session root + agent dirs exist.
		if err := fsq.EnsureRootDirs(root); err != nil {
			return fmt.Errorf("failed to create session root %q: %w", root, err)
		}
		if err := fsq.EnsureAgentDirs(root, agentHandle); err != nil {
			return fmt.Errorf("failed to create mailbox for %s at %q: %w", agentHandle, root, err)
		}
	}

	// Pin the session root to an absolute path before it reaches the wake
	// process and the exported AM_ROOT/AM_BASE_ROOT. A relative root
	// re-resolves against every future cwd of the agent process, silently
	// splitting one session name into per-directory mailbox trees
	// (messages land where no peer is reading).
	root, err = absoluteSessionRoot(root)
	if err != nil {
		return fmt.Errorf("resolve absolute session root: %w", err)
	}

	// Ensure agent mailbox exists.
	if err := fsq.EnsureAgentDirs(root, agentHandle); err != nil {
		return fmt.Errorf("failed to ensure mailbox for %s: %w", agentHandle, err)
	}

	// Resolve command binary.
	binaryPath, err := exec.LookPath(cmdName)
	if err != nil {
		return fmt.Errorf("command not found: %s", cmdName)
	}

	// Start amq wake in background (unless --no-wake).
	// On successful Exec, wake is orphaned (reparented to init/launchd) — intended.
	// On failed Exec, deferred kill cleans up the wake process.
	var wakeProc *os.Process
	if !*noWakeFlag {
		amqBin, binErr := os.Executable()
		if binErr != nil {
			amqBin = "amq"
		}

		var wakeOwner *wakeOwner
		if wakeInjectVia != "" {
			wakeOwner = currentWakeOwner()
		}
		wakeCmd := exec.Command(amqBin, buildCoopWakeArgs(agentHandle, root, wakeInjectMode, wakeInjectVia, []string(wakeInjectArgFlags))...)
		var readyPath string
		var cleanupReady func()
		if *requireWakeFlag {
			var readyErr error
			readyPath, cleanupReady, readyErr = newWakeReadyFile()
			if readyErr != nil {
				return fmt.Errorf("create wake readiness file: %w", readyErr)
			}
			defer cleanupReady()
			wakeCmd.Args = append(wakeCmd.Args, "--ready-file", readyPath, "--accept-existing-wake")
		}
		// Set AM_ROOT in wake's env so the helper process resolves the same
		// session root even if the parent shell inherited a different value.
		wakeEnv, wakeEnvErr := wakeCommandEnv(os.Environ(), root, wakeOwner)
		if wakeEnvErr != nil {
			return wakeEnvErr
		}
		wakeCmd.Env = wakeEnv
		wakeCmd.Stdin = os.Stdin
		wakeCmd.Stdout = os.Stdout
		wakeCmd.Stderr = os.Stderr

		if err := wakeCmd.Start(); err != nil {
			if *requireWakeFlag {
				return fmt.Errorf("start required amq wake: %w", err)
			}
			_ = writeStderr("warning: failed to start amq wake: %v\n", err)
		} else {
			wakeProc = wakeCmd.Process
			if *requireWakeFlag {
				if err := waitForWakeReady(wakeProc, readyPath, root, agentHandle, wakeReadyTimeout); err != nil {
					_ = wakeProc.Kill()
					return err
				}
			}
			_ = writeStderr("%s\n", wakeReadyMessage(root, agentHandle, wakeProc.Pid))
		}
	}

	// A named/default or session-shaped explicit root pins an identity
	// independent of AM_ROOT. A custom sessionless --root clears inherited pins.
	sessionIdentity := coopSessionIdentity(root, *sessionFlag, *rootFlag)
	env := buildCoopExecEnvironment(os.Environ(), root, agentHandle, sessionIdentity)

	// Build argv: command name + agent args.
	argv := append([]string{cmdName}, agentArgs...)

	// Replace process. On success, this never returns.
	// On failure, clean up the wake process.
	execErr := syscall.Exec(binaryPath, argv, env)
	if wakeProc != nil {
		_ = wakeProc.Kill()
	}
	return execErr
}

func buildCoopWakeArgs(agentHandle, root, injectMode, injectVia string, injectArgs []string) []string {
	args := []string{"--no-update-check", "wake", "--me", agentHandle, "--root", root, "--baseline-existing"}
	if injectMode != "" && injectMode != wakeInjectModeAuto {
		args = append(args, "--inject-mode", injectMode)
	}
	if injectVia != "" {
		args = append(args, "--inject-via", injectVia)
		for _, arg := range injectArgs {
			args = append(args, "--inject-arg", arg)
		}
	}
	return args
}

func newWakeReadyFile() (string, func(), error) {
	dir, err := os.MkdirTemp("", "amq-wake-ready-")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, "ready"), func() { _ = os.RemoveAll(dir) }, nil
}

func waitForWakeReady(proc *os.Process, readyPath, root, me string, timeout time.Duration) error {
	if proc == nil {
		return fmt.Errorf("amq wake process missing")
	}
	done := make(chan error, 1)
	go func() {
		_, err := proc.Wait()
		done <- err
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		if ready, err := validateWakeReadyFileAgainstCurrent(root, me, readyPath); err != nil {
			return fmt.Errorf("validate wake readiness: %w", err)
		} else if ready {
			return nil
		}

		select {
		case err := <-done:
			if ready, readyErr := validateWakeReadyFileAgainstCurrent(root, me, readyPath); readyErr != nil {
				return fmt.Errorf("validate wake readiness: %w", readyErr)
			} else if ready {
				return nil
			}
			if err != nil {
				return fmt.Errorf("amq wake exited before becoming ready: %w", err)
			}
			return fmt.Errorf("amq wake exited before becoming ready")
		case <-timer.C:
			return fmt.Errorf("amq wake did not become ready within %s", timeout)
		case <-ticker.C:
		}
	}
}

func wakeReadyMessage(root, agentHandle string, startedPID int) string {
	if inspection := inspectWakeLock(root, agentHandle); inspection.Status == wakeLockValid && inspection.PID != 0 && inspection.PID != startedPID {
		return fmt.Sprintf("Using existing amq wake (pid %d)", inspection.PID)
	}
	return fmt.Sprintf("Started amq wake (pid %d)", startedPID)
}

// splitDashDash splits args at the first "--" separator.
// Returns (before, after) where "--" itself is excluded from both.
func splitDashDash(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}
