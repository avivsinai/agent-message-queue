//go:build darwin || linux

package cli

import (
	"errors"
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

const wakeReadyTimeout = 25 * time.Second

const wakeProcessExitTimeout = 5 * time.Second

var killWakeHelperProcess = func(proc *os.Process) error { return proc.Kill() }

var coopExecProcess = syscall.Exec

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
		"Wake readiness:",
		"  Coop never reuses a generic wake because it has no persisted",
		"  exact-owner identity. Only an exact owner-bound inject-via wake can be",
		"  reused; stop an older generic wake before retrying coop exec.",
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

	// Start amq wake in background (unless --no-wake). Every coop-started wake
	// is bound to this exact process identity and exits when the exec-replaced
	// agent exits. On failed Exec, stable child cleanup stops it immediately.
	var wakeProc *os.Process
	var wakeWaiter *wakeProcessWaiter
	var wakeChildCapability *authoritativeWakeChildCapability
	var wakeHelperClaim *wakeLockInspection
	var cleanupWakeReady func()
	var earlyOwner *wakeOwner
	baseEnv := unsetEnvVar(unsetEnvVar(os.Environ(), envWakeOwner), envWakePrivateStopFD)
	retainWakeHelperClaim := func(current wakeLockInspection) {
		if retained := exactCoopWakeHelperClaim(wakeProc, current); retained != nil {
			wakeHelperClaim = retained
		}
	}
	cleanupWakeHelper := func(preservePersistedClaim bool) error {
		return cleanupCoopWakeStartupHelper(
			wakeProc,
			wakeWaiter,
			wakeChildCapability,
			earlyOwner,
			root,
			agentHandle,
			preservePersistedClaim,
			wakeHelperClaim,
		)
	}
	if !*noWakeFlag {
		amqBin, binErr := os.Executable()
		if binErr != nil {
			amqBin = "amq"
		}

		captured, ownerErr := captureAuthoritativeCurrentWakeOwner()
		if ownerErr != nil {
			return fmt.Errorf("capture exact coop wake owner: %w", ownerErr)
		}
		earlyOwner = &captured
		readyPath, cleanupReady, readyErr := newWakeReadyFile()
		if readyErr != nil {
			inspection := inspectWakeLock(root, agentHandle)
			if err := handleCoopWakeSetupFailure(*requireWakeFlag, inspection, "create wake readiness file", readyErr); err != nil {
				return err
			}
		} else {
			cleanupWakeReady = cleanupReady
			defer cleanupReady()
			wakeCmd := exec.Command(amqBin, buildCoopWakeArgs(agentHandle, root, wakeInjectMode, wakeInjectVia, []string(wakeInjectArgFlags), readyPath)...)
			// Set AM_ROOT in wake's env so the helper process resolves the same
			// session root even if the parent shell inherited a different value.
			wakeEnv, wakeEnvErr := wakeCommandEnv(baseEnv, root, earlyOwner)
			if wakeEnvErr != nil {
				return wakeEnvErr
			}
			wakeCmd.Env = wakeEnv
			wakeCmd.Stdin = os.Stdin
			wakeCmd.Stdout = os.Stdout
			wakeCmd.Stderr = os.Stderr
			wakeChildCapability, err = configureAuthoritativeWakeChild(wakeCmd)
			if err == nil && wakeChildCapability == nil {
				return fmt.Errorf("prepare exact-owner amq wake supervision returned nil capability")
			}
			if err != nil {
				var closeErr error
				if wakeChildCapability != nil {
					closeErr = wakeChildCapability.Close()
					wakeChildCapability = nil
				}
				if closeErr != nil {
					return errors.Join(
						fmt.Errorf("prepare exact-owner amq wake supervision: %w", err),
						fmt.Errorf("cleanup unstarted coop wake capability: %w", closeErr),
					)
				}
				inspection := inspectWakeLock(root, agentHandle)
				if setupErr := handleCoopWakeSetupFailure(
					*requireWakeFlag,
					inspection,
					"prepare exact-owner amq wake supervision",
					err,
				); setupErr != nil {
					return setupErr
				}
			} else if err := wakeCmd.Start(); err != nil {
				var closeErr error
				if wakeChildCapability != nil {
					closeErr = wakeChildCapability.Close()
					wakeChildCapability = nil
				}
				if closeErr != nil {
					return errors.Join(
						fmt.Errorf("start exact-owner amq wake helper: %w", err),
						fmt.Errorf("cleanup unstarted coop wake capability: %w", closeErr),
					)
				}
				inspection := inspectWakeLock(root, agentHandle)
				if err := handleCoopWakeSetupFailure(*requireWakeFlag, inspection, "start exact-owner amq wake helper", err); err != nil {
					return err
				}
			} else {
				wakeProc = wakeCmd.Process
				wakeWaiter = newWakeProcessWaiter(wakeProc)
				if wakeChildCapability != nil {
					if err := wakeChildCapability.Bind(wakeProc); err != nil {
						current := inspectWakeLock(root, agentHandle)
						retainWakeHelperClaim(current)
						cleanupErr := cleanupWakeHelper(current.Exists && current.PID != wakeProc.Pid)
						bindErr := fmt.Errorf("bind stable owner-bound wake child: %w", err)
						if cleanupErr == nil {
							return bindErr
						}
						return errors.Join(bindErr, fmt.Errorf("cleanup exact coop wake startup helper: %w", cleanupErr))
					}
				}
				readyErr := waitForWakeReadyWithOwner(
					wakeWaiter,
					readyPath,
					root,
					agentHandle,
					earlyOwner,
					wakeReadyTimeout,
				)
				current := inspectWakeLock(root, agentHandle)
				retainWakeHelperClaim(current)
				otherWake := current.Exists && current.PID != wakeProc.Pid
				if readyErr != nil {
					cleanupErr := cleanupWakeHelper(otherWake)
					if cleanupErr != nil {
						return errors.Join(
							readyErr,
							fmt.Errorf("cleanup exact coop wake startup helper: %w", cleanupErr),
						)
					}
					if otherWake || *requireWakeFlag {
						return readyErr
					}
					_ = writeStderr("warning: failed to prepare amq wake: %v\n", readyErr)
					wakeProc = nil
					wakeWaiter = nil
					wakeChildCapability = nil
				} else {
					current, claimErr := validatePreparedCoopWakeClaim(
						root,
						agentHandle,
						wakeInjectVia,
						[]string(wakeInjectArgFlags),
						*earlyOwner,
						wakeProc.Pid,
					)
					reused := current.Exists && current.PID != wakeProc.Pid
					if !reused {
						retainWakeHelperClaim(current)
					}
					if claimErr != nil {
						cleanupErr := cleanupWakeHelper(reused)
						if cleanupErr != nil {
							return errors.Join(
								claimErr,
								fmt.Errorf("cleanup exact coop wake startup helper: %w", cleanupErr),
							)
						}
						return claimErr
					}
					if reused {
						if cleanupErr := cleanupWakeHelper(true); cleanupErr != nil {
							return fmt.Errorf(
								"finish exact reused-wake startup helper: %w",
								cleanupErr,
							)
						}
						_ = writeStderr("%s\n", wakeReadyMessage(root, agentHandle, current.PID))
						wakeProc = nil
						wakeWaiter = nil
						wakeChildCapability = nil
					} else {
						_ = writeStderr("%s\n", wakeReadyMessage(root, agentHandle, wakeProc.Pid))
					}
				}
			}
		}
	}

	// A named/default or session-shaped explicit root pins an identity
	// independent of AM_ROOT. A custom sessionless --root clears inherited pins.
	sessionIdentity := coopSessionIdentity(root, *sessionFlag, *rootFlag)
	env := buildCoopExecEnvironment(baseEnv, root, agentHandle, sessionIdentity)

	// Build argv: command name + agent args.
	argv := append([]string{cmdName}, agentArgs...)

	// Replace process. On success, this never returns.
	// On failure, clean up the wake process.
	if cleanupWakeReady != nil {
		cleanupWakeReady()
	}
	cleanupAfterError := func(cause error) error {
		cleanupErr := cleanupWakeHelper(false)
		if cleanupErr == nil {
			return cause
		}
		return errors.Join(
			cause,
			fmt.Errorf("cleanup exact coop wake helper: %w", cleanupErr),
		)
	}
	finalOwner, ownerErr := captureAuthoritativeCurrentWakeOwner()
	if ownerErr != nil {
		return cleanupAfterError(fmt.Errorf("capture final coop wake owner: %w", ownerErr))
	}
	if earlyOwner != nil && *earlyOwner != finalOwner {
		return cleanupAfterError(fmt.Errorf("coop exec process identity changed after owner-bound wake start"))
	}
	encodedOwner, ownerErr := encodeWakeOwnerEnv(finalOwner)
	if ownerErr != nil {
		return cleanupAfterError(fmt.Errorf("encode final wake owner: %w", ownerErr))
	}
	env = setEnvVar(unsetEnvVar(env, envWakeOwner), envWakeOwner, encodedOwner)
	execErr := coopExecProcess(binaryPath, argv, env)
	if execErr == nil {
		execErr = fmt.Errorf("exec returned without replacing process")
	}
	return cleanupAfterError(execErr)
}

func exactCoopWakeHelperClaim(
	proc *os.Process,
	current wakeLockInspection,
) *wakeLockInspection {
	if proc == nil ||
		!current.Exists ||
		current.PID != proc.Pid ||
		current.Lock.Generation == "" {
		return nil
	}
	switch current.Status {
	case wakeLockValid, wakeLockStale:
	default:
		return nil
	}
	switch classifyPersistedWakeClaim(current) {
	case wakeClaimGeneric, wakeClaimAuthoritative:
		retained := current
		return &retained
	default:
		return nil
	}
}

func validatePreparedCoopWakeClaim(
	root string,
	me string,
	injectVia string,
	injectArgs []string,
	owner wakeOwner,
	helperPID int,
) (wakeLockInspection, error) {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return wakeLockInspection{}, err
	}
	defer func() { _ = agentDir.Close() }()

	var current wakeLockInspection
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current = inspectWakeLockAt(dirfd, agentDir, root, me)
		if !confirmedLiveWake(current) {
			return fmt.Errorf("prepared coop wake is not a confirmed live wake")
		}
		if current.PID != helperPID && injectVia == "" {
			return fmt.Errorf(
				"generic coop wake readiness resolved to unrelated pid %d",
				current.PID,
			)
		}
		if injectVia == "" {
			return nil
		}
		if classifyPersistedWakeClaim(current) != wakeClaimAuthoritative {
			return fmt.Errorf("prepared inject-via wake is not an authoritative owner claim")
		}
		target, err := validateAuthoritativeWakeClaimPairAt(dirfd, agentDir, current)
		if err != nil {
			return fmt.Errorf("validate prepared inject-via owner claim: %w", err)
		}
		if !sameWakeOwner(current.Lock.Owner, &owner) ||
			!sameWakeOwner(target.Owner, &owner) {
			return fmt.Errorf("prepared inject-via wake belongs to a different exact owner")
		}
		requested, err := newWakeTarget(root, me, injectVia, injectArgs)
		if err != nil {
			return fmt.Errorf("rebuild requested inject-via target: %w", err)
		}
		requested.Owner = &owner
		if !sameWakeInjectorIdentity(target, requested) {
			return fmt.Errorf("prepared inject-via wake uses a different injector path or fixed arguments")
		}
		return nil
	})
	return current, err
}

func cleanupCoopWakeStartupHelper(
	proc *os.Process,
	waiter *wakeProcessWaiter,
	capability *authoritativeWakeChildCapability,
	owner *wakeOwner,
	root string,
	me string,
	preservePersistedClaim bool,
	helperClaim *wakeLockInspection,
) error {
	if proc == nil {
		if capability == nil {
			return nil
		}
		return capability.Close()
	}
	if !preservePersistedClaim {
		return cleanupStartedWakeHelper(proc, waiter, capability, owner, root, me, helperClaim)
	}
	if waiter == nil {
		return fmt.Errorf("coop wake startup helper waiter is missing")
	}
	if capability == nil {
		return fmt.Errorf("coop wake startup helper capability is missing")
	}
	// The persisted claim belongs to another exact process. Stop and join only
	// this startup helper; never run claim cleanup against the reused generation.
	stopErr := capability.Stop()
	waitErr := waiter.waitForExit(wakeProcessExitTimeout)
	closeErr := capability.Close()
	return errors.Join(stopErr, waitErr, closeErr)
}

func confirmedLiveWake(inspection wakeLockInspection) bool {
	return inspection.Exists &&
		inspection.Status == wakeLockValid &&
		inspection.IdentityConfirmed &&
		inspection.Process.Running
}

func handleCoopWakeSetupFailure(requireWake bool, inspection wakeLockInspection, action string, cause error) error {
	if requireWake || confirmedLiveWake(inspection) {
		return fmt.Errorf("%s: %w", action, cause)
	}
	_ = writeStderr("warning: %s: %v\n", action, cause)
	return nil
}

func buildCoopWakeArgs(agentHandle, root, injectMode, injectVia string, injectArgs []string, readyFile string) []string {
	args := []string{
		"--no-update-check",
		"wake",
		"--me", agentHandle,
		"--root", root,
		"--baseline-existing",
		"--interrupt-cmd", "none",
	}
	if injectMode != "" && injectMode != wakeInjectModeAuto {
		args = append(args, "--inject-mode", injectMode)
	}
	if injectVia != "" {
		args = append(args, "--inject-via", injectVia)
		for _, arg := range injectArgs {
			args = append(args, "--inject-arg", arg)
		}
	}
	if readyFile != "" {
		args = append(args, "--ready-file", readyFile)
		if injectVia != "" {
			// Only an exact owner-bound inject-via claim has persisted owner
			// metadata that can prove reuse belongs to this coop process.
			args = append(args, "--accept-existing-wake")
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

type wakeProcessWaiter struct {
	done  chan struct{}
	state *os.ProcessState
	err   error
}

func newWakeProcessWaiter(proc *os.Process) *wakeProcessWaiter {
	waiter := &wakeProcessWaiter{done: make(chan struct{})}
	go func() {
		waiter.state, waiter.err = proc.Wait()
		close(waiter.done)
	}()
	return waiter
}

func (waiter *wakeProcessWaiter) waitForExit(timeout time.Duration) error {
	if waiter == nil {
		return fmt.Errorf("amq wake process waiter missing")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-waiter.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("amq wake did not exit within %s", timeout)
	}
}

func waitForWakeReady(proc *os.Process, readyPath, root, me string, timeout time.Duration) error {
	if proc == nil {
		return fmt.Errorf("amq wake process missing")
	}
	return waitForWakeReadyWithWaiter(newWakeProcessWaiter(proc), readyPath, root, me, timeout)
}

func waitForWakeReadyWithWaiter(waiter *wakeProcessWaiter, readyPath, root, me string, timeout time.Duration) error {
	return waitForWakeReadyWithOwner(waiter, readyPath, root, me, nil, timeout)
}

func waitForWakeReadyWithOwner(
	waiter *wakeProcessWaiter,
	readyPath string,
	root string,
	me string,
	requestedOwner *wakeOwner,
	timeout time.Duration,
) error {
	if waiter == nil {
		return fmt.Errorf("amq wake process waiter missing")
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		if ready, err := validateWakeReadyFileAgainstOwner(root, me, readyPath, requestedOwner); err != nil {
			return fmt.Errorf("validate wake readiness: %w", err)
		} else if ready {
			return nil
		}

		select {
		case <-waiter.done:
			if ready, readyErr := validateWakeReadyFileAgainstOwner(root, me, readyPath, requestedOwner); readyErr != nil {
				return fmt.Errorf("validate wake readiness: %w", readyErr)
			} else if ready {
				return nil
			}
			if waiter.err != nil {
				return fmt.Errorf("amq wake exited before becoming ready: %w", waiter.err)
			}
			return fmt.Errorf("amq wake exited before becoming ready")
		case <-timer.C:
			return fmt.Errorf("amq wake did not become ready within %s", timeout)
		case <-ticker.C:
		}
	}
}

func terminateWakeHelperProcess(proc *os.Process, waiter *wakeProcessWaiter, root, me string) error {
	if proc == nil || waiter == nil {
		return nil
	}
	expected := inspectWakeLock(root, me)
	ownedGeneration := confirmedLiveWake(expected) && expected.PID == proc.Pid && expected.Lock.Generation != ""
	_ = killWakeHelperProcess(proc)
	if err := waiter.waitForExit(wakeProcessExitTimeout); err != nil {
		return err
	}
	if ownedGeneration {
		return cleanupTerminatedWakeLock(expected)
	}
	return cleanupTerminatedWakeLockForPID(root, me, proc.Pid)
}

func cleanupStartedWakeHelper(
	proc *os.Process,
	waiter *wakeProcessWaiter,
	capability *authoritativeWakeChildCapability,
	owner *wakeOwner,
	root string,
	me string,
	helperClaim *wakeLockInspection,
) error {
	if owner == nil {
		return terminateWakeHelperProcess(proc, waiter, root, me)
	}
	return terminateAuthoritativeWakeHelperProcessForClaim(
		proc,
		waiter,
		capability,
		root,
		me,
		*owner,
		helperClaim,
	)
}

func terminateAuthoritativeWakeHelperProcess(
	proc *os.Process,
	waiter *wakeProcessWaiter,
	capability *authoritativeWakeChildCapability,
	root string,
	me string,
	owner wakeOwner,
) error {
	expected := inspectWakeLock(root, me)
	return terminateAuthoritativeWakeHelperProcessForClaim(
		proc,
		waiter,
		capability,
		root,
		me,
		owner,
		&expected,
	)
}

func terminateAuthoritativeWakeHelperProcessForClaim(
	proc *os.Process,
	waiter *wakeProcessWaiter,
	capability *authoritativeWakeChildCapability,
	root string,
	me string,
	owner wakeOwner,
	helperClaim *wakeLockInspection,
) error {
	if capability == nil {
		return fmt.Errorf("stable owner-bound wake child capability is missing")
	}
	stopErr := capability.Stop()
	var waitErr error
	switch {
	case proc == nil:
		waitErr = fmt.Errorf("stable owner-bound wake child process is missing")
	case waiter == nil:
		waitErr = fmt.Errorf("stable owner-bound wake child waiter is missing")
	default:
		waitErr = waiter.waitForExit(wakeProcessExitTimeout)
	}
	closeErr := capability.Close()

	var claimErr error
	if waitErr == nil && helperClaim != nil {
		switch classifyPersistedWakeClaim(*helperClaim) {
		case wakeClaimAuthoritative:
			claimErr = rollbackAuthoritativeWakeClaimForInspection(root, me, owner, *helperClaim)
		case wakeClaimGeneric:
			claimErr = cleanupTerminatedWakeLock(*helperClaim)
		case wakeClaimAbsent:
		default:
			claimErr = fmt.Errorf("retained helper wake claim is unverified; preserving it")
		}
	}
	return errors.Join(stopErr, waitErr, closeErr, claimErr)
}

func rollbackAuthoritativeWakeClaim(root, me string, owner wakeOwner) error {
	expected := inspectWakeLock(root, me)
	return rollbackAuthoritativeWakeClaimForInspection(root, me, owner, expected)
}

func rollbackAuthoritativeWakeClaimForInspection(
	root string,
	me string,
	owner wakeOwner,
	expected wakeLockInspection,
) error {
	currentOwner, err := captureAuthoritativeCurrentWakeOwner()
	if err != nil {
		return err
	}
	if currentOwner != owner {
		return fmt.Errorf("current process is not the exact owner authorized for wake rollback")
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !current.Exists {
			return nil
		}
		if !sameWakeLockGeneration(expected, current) {
			return nil
		}
		if classifyPersistedWakeClaim(current) != wakeClaimAuthoritative ||
			!sameWakeOwner(current.Lock.Owner, &owner) {
			return fmt.Errorf("wake claim changed before exact owner rollback")
		}
		target, err := validateAuthoritativeWakeClaimPairAt(dirfd, agentDir, current)
		if err != nil {
			return err
		}
		if current.Status != wakeLockStale {
			return fmt.Errorf("owner-bound wake is not conclusively absent after helper stop")
		}
		return removeAuthoritativeWakeClaimAt(dirfd, agentDir, current, &target)
	})
}

func cleanupTerminatedWakeLock(expected wakeLockInspection) error {
	return withWakeLifecycleGuard(expected.Root, expected.Agent, func() error {
		current := inspectWakeLock(expected.Root, expected.Agent)
		if !sameWakeLockGeneration(expected, current) {
			return nil
		}
		if current.Status != wakeLockStale {
			return fmt.Errorf("terminated wake lock is not proven stale: %s", current.Status)
		}
		if err := validateWakeLockStaleRemoval(current); err != nil {
			return err
		}
		return removeWakeLockIfUnchangedGuarded(current)
	})
}

func cleanupTerminatedWakeLockForPID(root, me string, terminatedPID int) error {
	return withWakeLifecycleGuard(root, me, func() error {
		current := inspectWakeLock(root, me)
		if !current.Exists || current.PID != terminatedPID || current.Lock.Generation == "" {
			return nil
		}
		if current.Status != wakeLockStale {
			return fmt.Errorf("terminated wake lock is not proven stale: %s", current.Status)
		}
		if err := validateWakeLockStaleRemoval(current); err != nil {
			return err
		}
		return removeWakeLockIfUnchangedGuarded(current)
	})
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
