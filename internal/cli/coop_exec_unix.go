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
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"golang.org/x/sys/unix"
)

const wakeReadyTimeout = 25 * time.Second

const wakeProcessExitTimeout = 5 * time.Second

func killWakeHelperProcessWithHandle(proc *os.Process) error { return killWakeProcess(proc) }

var killWakeHelperProcess = killWakeHelperProcessWithHandle

var coopExecProcess = syscall.Exec

var openCoopWakeTTY = func() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_WRONLY|unix.O_CLOEXEC, 0)
}

func openCoopWakeAttention(output *os.File) (*os.File, error) {
	if attention, err := openCoopWakeTTY(); err == nil {
		return attention, nil
	}
	if output == nil {
		return nil, fmt.Errorf("wake diagnostics are unavailable")
	}
	fd, err := unix.Dup(int(output.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate wake diagnostics for non-terminal attention: %w", err)
	}
	unix.CloseOnExec(fd)
	attention := os.NewFile(uintptr(fd), output.Name())
	if attention == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("adopt non-terminal wake attention descriptor")
	}
	return attention, nil
}

func runCoopExec(args []string) error {
	managedLaunchNonce := strings.TrimSpace(os.Getenv(launch.InternalLaunchNonceEnv))
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
	managedNoWakeReasonFlag := fs.String("managed-no-wake-reason", "", "")
	var managedSymphonyEventFlags multiStringFlag
	fs.Var(&managedSymphonyEventFlags, "managed-symphony-event", "")
	managedSymphonyWorkspaceFlag := fs.String("managed-symphony-workspace-key", "", "")
	yesFlag := fs.Bool("y", false, "Skip confirmation prompts (including clearing a blocking wake)")
	namedFlag := fs.Bool("named", true, "Stamp the session and AM_ME onto the spawned CLI session name")

	usage := usageWithFlags(fs, "amq coop exec [options] <command> [-- <command-flags>]",
		"Set up co-op mode and exec into the agent (replaces this process).",
		"",
		"Sets AM_ROOT (always a session subdirectory) and AM_ME,",
		"starts amq wake in background, then",
		"replaces itself with the given command via exec.",
		"",
		"If neither --session nor --root is given, defaults to the declared",
		"default_session from .amq/launch.json, or collab when none is declared.",
		"The agent handle is derived from the command basename unless --me is set.",
		"",
		"Examples:",
		"  amq coop exec claude                              # Exec into Claude Code (declared session or collab)",
		"  amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Codex with flags",
		"  amq coop exec grok                                # Grok CLI, caller flags forwarded as-is",
		"  amq coop exec --session feature-x claude          # Isolated session",
		"  amq coop exec --root .agent-mail/auth claude      # Explicit root (no session default)",
		"  amq coop exec --require-wake --wake-inject-mode none claude  # Zero-input wake",
		"  amq coop exec --wake-inject-via /path/to/injector codex",
		"  amq coop exec --me myagent bash                   # Debug shell with AMQ env",
		"  amq coop exec --named=false claude              # Disable automatic session naming",
		"",
		"Wake readiness:",
		"  Coop never reuses a generic wake because it has no persisted",
		"  exact-owner identity. Only an exact owner-bound inject-via wake can be",
		"  reused; stop an older generic wake before retrying coop exec.",
	)

	if handled, err := parseFlagsAllowPositionals(fs, amqArgs, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	namedEnabled, err := resolveCoopNamedEnabled(flagWasVisited(fs, "named"), *namedFlag)
	if err != nil {
		return err
	}
	if managedLaunchNonce != "" {
		// Managed launches use the ticket's exact provider argv for naming.
		namedEnabled = false
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
	if managedLaunchNonce != "" && flagWasVisited(fs, "named") && *namedFlag && !launch.ArgsHaveNameFlag(agentArgs) {
		return UsageError("--named is not supported under a managed launch; declare the name in the launch plan instead")
	}
	if managedLaunchNonce != "" {
		if !flagWasVisited(fs, "root") || strings.TrimSpace(*rootFlag) == "" || !filepath.IsAbs(*rootFlag) {
			return ActionRequiredError("managed launch requires one explicit absolute --root")
		}
		if *sessionFlag != "" {
			return ActionRequiredError("managed launch forbids --session; use the ticket-bound exact --root")
		}
	}
	if *noWakeFlag && *requireWakeFlag {
		return UsageError("--require-wake cannot be used with --no-wake")
	}
	declaredWakeInjectVia := strings.TrimSpace(*wakeInjectViaFlag)
	wakeInjectVia := declaredWakeInjectVia
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
	managedOnlyOptions := flagWasVisited(fs, "managed-no-wake-reason") ||
		flagWasVisited(fs, "managed-symphony-event") ||
		flagWasVisited(fs, "managed-symphony-workspace-key")
	if managedOnlyOptions && managedLaunchNonce == "" {
		return UsageError("managed execution options require a trusted launch ticket")
	}
	var executionOptions *launch.PrepareExecutionOptions
	if managedLaunchNonce != "" && (managedOnlyOptions || flagWasVisited(fs, "named") ||
		flagWasVisited(fs, "no-gitignore") || flagWasVisited(fs, "no-wake") ||
		flagWasVisited(fs, "require-wake") || flagWasVisited(fs, "wake-inject-mode") ||
		flagWasVisited(fs, "wake-inject-via") || flagWasVisited(fs, "wake-inject-arg")) {
		wakeMode := "enabled"
		if *noWakeFlag {
			wakeMode = "disabled"
		}
		managedInjectorMode := ""
		if flagWasVisited(fs, "wake-inject-mode") {
			managedInjectorMode = wakeInjectMode
		}
		options := launch.PrepareExecutionOptions{
			RequireWake:          *requireWakeFlag,
			NoGitignore:          *noGitignoreFlag,
			Named:                flagWasVisited(fs, "named") && *namedFlag,
			WakeMode:             wakeMode,
			AuditReason:          *managedNoWakeReasonFlag,
			InjectorMode:         managedInjectorMode,
			InjectorVia:          declaredWakeInjectVia,
			InjectorArgs:         append([]string(nil), wakeInjectArgFlags...),
			SymphonyEvents:       append([]string(nil), managedSymphonyEventFlags...),
			SymphonyWorkspaceKey: *managedSymphonyWorkspaceFlag,
		}
		if err := validateManagedExecutionOptions(options); err != nil {
			return UsageError("managed execution options: %v", err)
		}
		executionOptions = &options
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
	var managedGuard *managedExecutionGuard
	if managedLaunchNonce != "" {
		managedGuard, err = acquireManagedExecutionGuard(*rootFlag, agentHandle, managedLaunchNonce, executionOptions)
		if err != nil {
			return ActionRequiredError("refusing managed execution options before root mutation: %v", err)
		}
		defer func() { _ = managedGuard.Close() }()
	}

	rootRequested := flagWasVisited(fs, "root")
	sessionRequested := *sessionFlag != ""
	selectorFree := !rootRequested && !sessionRequested
	defaultSession := defaultSessionName
	var warnedCreation bool

	// Resolve explicit --session (pure sugar for --root <base>/<session>).
	// A fresh Git worktree has no base yet; remember that bootstrap is needed
	// and provision the requested session after full coop init completes.
	bootstrapBase := ""
	if *sessionFlag != "" {
		if flagWasVisited(fs, "root") {
			return UsageError("--session and --root are mutually exclusive")
		}
		if err := validateSessionName(*sessionFlag); err != nil {
			return err
		}
		base, ambient, err := resolveCoopAmbientSessionBase()
		if err != nil {
			return err
		}
		if !ambient {
			if *noInitFlag {
				base, err = resolveBaseRoot()
				if err != nil {
					return err
				}
			} else {
				var found bool
				base, found, err = resolveDiscoveredBaseRootForBootstrap()
				if err != nil {
					return err
				}
				if !found {
					bootstrapBase = base
					base = ""
				}
			}
		}
		if base != "" {
			*rootFlag = filepath.Join(base, *sessionFlag)
		}
	}

	// Resolve root: --root flag (or --session-derived) > ambient context >
	// project/repo-local/global discovery > local initialization.
	root := *rootFlag
	sessionProvisioned := false
	if root == "" {
		ambientBase, ambient, ambientErr := resolveCoopAmbientSessionBase()
		if ambientErr != nil {
			return ambientErr
		}
		if ambient {
			root = ambientBase
		} else {
			var discoveredBase string
			var found bool
			var discoveryErr error
			if *noInitFlag {
				discoveredBase, found, discoveryErr = resolveDiscoveredBaseRoot()
			} else {
				discoveredBase, found, discoveryErr = resolveDiscoveredBaseRootForBootstrap()
			}
			if discoveryErr != nil {
				return discoveryErr
			}
			if found {
				root = discoveredBase
			} else if discoveredBase != "" {
				bootstrapBase = discoveredBase
			}
		}
	}

	// Resolve the command binary after every pure flag/name validation but
	// before the first provisioning mutation: a typo'd command must fail
	// without minting roots, sessions, or mailboxes, while usage errors keep
	// their established precedence over command-not-found.
	binaryPath, err := exec.LookPath(cmdName)
	if err != nil {
		return fmt.Errorf("command not found: %s", cmdName)
	}

	if selectorFree {
		defaultSession, err = declaredCoopExecSession()
		if err != nil {
			return err
		}
	}
	creationDeprecated, err := hasBootstrapSuppressingDeclaration()
	if err != nil {
		return err
	}

	// Explicit named sessions use the same direct-child creation boundary as
	// coop init/default exec. In particular, never let MkdirAll traverse a
	// pre-existing session symlink.
	if *sessionFlag != "" && root != "" {
		if *noInitFlag && !dirExists(root) {
			return fmt.Errorf("root %q does not exist; run 'amq coop init' first or remove --no-init", root)
		}
		base := filepath.Dir(root)
		if !dirExists(base) {
			if err := fsq.EnsureRootDirs(base); err != nil {
				return fmt.Errorf("failed to create base root %q: %w", base, err)
			}
		}
		requestedRoot := root
		if !dirExists(requestedRoot) {
			warnCoopExecCreationDeprecated(&warnedCreation)
		}
		root, err = provisionCoopSession(base, *sessionFlag, []string{agentHandle}, agentHandle, cmdName)
		if err != nil {
			return fmt.Errorf("failed to create session root %q: %w", requestedRoot, err)
		}
		sessionProvisioned = true
	}

	// Auto-init if needed (before session defaulting so full init fires on fresh projects).
	if root == "" || !dirExists(root) {
		if *noInitFlag {
			if root == "" {
				return fmt.Errorf("no project, repo-local, or global AMQ root found and no --root specified; run 'amq coop init' first or remove --no-init")
			}
			return fmt.Errorf("root %q does not exist; run 'amq coop init' first or remove --no-init", root)
		}

		if root != "" {
			// We have a root (from --root, --session, or .amqrc) — create root + agent dirs.
			if rootRequested {
				warnCoopExecCreationDeprecated(&warnedCreation)
			}
			delivery, err := fsq.CreateDirectoryPathExclusive(root)
			if err != nil {
				return fmt.Errorf("failed to create root %q: %w", root, err)
			}
			if err := delivery.EnsureRootDirs(); err != nil {
				_ = delivery.Close()
				return fmt.Errorf("failed to create root %q: %w", root, err)
			}
			if err := delivery.EnsureAgentDirs(agentHandle); err != nil {
				_ = delivery.Close()
				return fmt.Errorf("failed to create mailbox for %s at %q: %w", agentHandle, root, err)
			}
			_ = delivery.Close()
		} else {
			// No explicit, ambient, project, repo-local, or global root: run
			// full coop init. In a Git worktree, coop init relocates this to
			// the worktree top so nested invocations do not scatter queues.
			if !*yesFlag {
				location := "current directory"
				if bootstrapBase != "" {
					location = fmt.Sprintf("Git worktree at %s", filepath.Dir(bootstrapBase))
				}
				ok, err := confirmPromptYes(fmt.Sprintf("No project, repo-local, or global AMQ root found. Initialize co-op mode in %s?", location))
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

	if *sessionFlag != "" && !sessionProvisioned {
		base := root
		if bootstrapBase != "" && !sameTreeIdentity(base, bootstrapBase) {
			return fmt.Errorf("bootstrap resolved base %q, but coop init produced %q", bootstrapBase, base)
		}
		requestedRoot := filepath.Join(base, *sessionFlag)
		if !dirExists(requestedRoot) {
			warnCoopExecCreationDeprecated(&warnedCreation)
		}
		root, err = provisionCoopSession(base, *sessionFlag, []string{agentHandle}, agentHandle, cmdName)
		if err != nil {
			return fmt.Errorf("failed to create session root %q: %w", requestedRoot, err)
		}
		sessionProvisioned = true
	}

	// Default to the declared session (or collab) when neither --session nor
	// --root was specified. This runs after auto-init so .amqrc exists.
	if selectorFree {
		base := root // root is the literal .amqrc root (e.g., .agent-mail)
		requestedRoot := filepath.Join(base, defaultSession)
		if creationDeprecated && !dirExists(requestedRoot) {
			warnCoopExecCreationDeprecated(&warnedCreation)
		}
		root, err = provisionCoopSession(base, defaultSession, []string{agentHandle}, agentHandle, cmdName)
		if err != nil {
			return fmt.Errorf("failed to create session root %q: %w", requestedRoot, err)
		}
		sessionProvisioned = true
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
	// Ensure the agent mailbox exists — but only inside a tree that is
	// provably a queue. A pre-existing directory is not a provisioning
	// target just because it exists (dirExists said nothing about what it
	// is), so classify the layout through a pinned capability first and keep
	// that same capability for the writes.
	if !sessionProvisioned {
		if managedGuard != nil {
			if err := fsq.ValidateExistingMailboxLayout(managedGuard.root, agentHandle); err != nil {
				return ActionRequiredError("refusing managed mailbox provisioning: %v", err)
			}
		} else {
			if err := provisionSelfMailboxInExistingRoot(root, agentHandle); err != nil {
				return err
			}
		}
	}
	if !*noWakeFlag {
		if err := prepareCoopWakeLock(
			root,
			agentHandle,
			*yesFlag,
			coopWakeRemedyForCommand(root, agentHandle, cmdName, agentArgs),
		); err != nil {
			return err
		}
	}

	// Start amq wake in background (unless --no-wake). Every coop-started wake
	// is bound to this exact process identity and exits when the exec-replaced
	// agent exits. On failed Exec, stable child cleanup stops it immediately.
	var wakeProc *os.Process
	var wakeWaiter *wakeProcessWaiter
	var wakeChildCapability *authoritativeWakeChildCapability
	var wakeHelperClaim *retainedCoopWakeHelperClaim
	var wakeHelperClaimErr error
	var cleanupWakeReady func()
	var earlyOwner *wakeOwner
	baseEnv := unsetEnvVar(
		unsetEnvVar(
			unsetEnvVar(
				unsetEnvVar(os.Environ(), launch.InternalLaunchNonceEnv),
				envWakeOwner,
			),
			envWakePrivateStopFD,
		),
		envWakeAttentionFD,
	)
	retainWakeHelperClaim := func() {
		retained, err := captureRetainedCoopWakeHelperClaim(wakeProc, root, agentHandle)
		if err != nil {
			wakeHelperClaimErr = errors.Join(wakeHelperClaimErr, err)
			return
		}
		if retained != nil {
			if wakeHelperClaim != nil {
				_ = wakeHelperClaim.Close()
			}
			wakeHelperClaim = retained
		}
	}
	cleanupWakeHelper := func(preservePersistedClaim bool) error {
		cleanupErr := cleanupCoopWakeStartupHelper(
			wakeProc,
			wakeWaiter,
			wakeChildCapability,
			earlyOwner,
			root,
			agentHandle,
			preservePersistedClaim,
			wakeHelperClaim,
		)
		wakeHelperClaim = nil
		return errors.Join(cleanupErr, wakeHelperClaimErr)
	}
	if !*noWakeFlag {
		amqExecutable, binErr := os.Executable()
		if binErr != nil {
			return fmt.Errorf("resolve current amq executable: %w", binErr)
		}
		amqBin, binErr := coopWakeExecutionPath(amqExecutable)
		if binErr != nil {
			return binErr
		}
		wakeLaunchLocator := coopWakeLaunchLocator(os.Args[0])

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
			if wakeLaunchLocator != "" {
				// Execute the already-running image, but preserve the absolute,
				// unresolved invocation locator as argv[0] so the child can safely
				// validate and observe installer symlink flips without a launch TOCTOU.
				wakeCmd.Args[0] = wakeLaunchLocator
			}
			// Set AM_ROOT in wake's env so the helper process resolves the same
			// session root even if the parent shell inherited a different value.
			wakeEnv, wakeEnvErr := wakeCommandEnv(baseEnv, root, earlyOwner)
			if wakeEnvErr != nil {
				return wakeEnvErr
			}
			wakeCmd.Env = wakeEnv
			wakeCmd.Stdin = os.Stdin
			wakeOutput, outputErr := openCoopWakeOutput(root, agentHandle)
			if outputErr != nil {
				return fmt.Errorf("open durable coop wake diagnostics: %w", outputErr)
			}
			defer func() { _ = wakeOutput.Close() }()
			wakeAttention, attentionErr := openCoopWakeAttention(wakeOutput)
			if attentionErr != nil {
				return fmt.Errorf("open isolated coop wake attention: %w", attentionErr)
			}
			defer func() { _ = wakeAttention.Close() }()
			wakeCmd.Stdout = wakeOutput
			wakeCmd.Stderr = wakeOutput
			if err := attachWakeAttentionFD(wakeCmd, wakeAttention); err != nil {
				return fmt.Errorf("attach isolated coop wake attention: %w", err)
			}
			wakeChildCapability, err = configureAuthoritativeWakeChild(wakeCmd)
			if err == nil && wakeChildCapability == nil {
				return fmt.Errorf("prepare exact-owner amq wake supervision returned nil capability")
			}
			if err == nil && managedGuard != nil {
				if validateErr := managedGuard.Revalidate(); validateErr != nil {
					closeErr := wakeChildCapability.Close()
					wakeChildCapability = nil
					return ActionRequiredError(
						"refusing changed managed execution options before wake start: %v",
						errors.Join(validateErr, closeErr),
					)
				}
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
						retainWakeHelperClaim()
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
				retainWakeHelperClaim()
				otherWake := current.Exists && current.PID != wakeProc.Pid
				if readyErr != nil {
					startupErr := readyErr
					if otherWake {
						startupErr = coopWakeStartupConflictError(current, readyErr)
					}
					cleanupErr := cleanupWakeHelper(otherWake)
					if cleanupErr != nil {
						return errors.Join(
							startupErr,
							fmt.Errorf("cleanup exact coop wake startup helper: %w", cleanupErr),
						)
					}
					if otherWake || *requireWakeFlag {
						return startupErr
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
						retainWakeHelperClaim()
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
	if managedGuard != nil {
		if err := managedGuard.Close(); err != nil {
			return ActionRequiredError("release managed execution option guard after wake startup: %v", err)
		}
	}

	// A named/default or session-shaped explicit root pins an identity
	// independent of AM_ROOT. A custom sessionless --root clears inherited pins.
	requestedSession := *sessionFlag
	if selectorFree {
		requestedSession = defaultSession
	}
	sessionIdentity := coopSessionIdentity(root, requestedSession, *rootFlag)
	env := buildCoopExecEnvironment(baseEnv, root, agentHandle, sessionIdentity)

	// Automatic naming uses the resolved session identity. A custom sessionless
	// root has no session prefix and therefore uses only the agent handle.
	namedLabel := coopNamedSessionLabel(sessionIdentity, agentHandle)
	execStart := time.Now()
	agentArgs, err = applyCoopNamedBeforeExecAt(namedEnabled, binaryPath, agentArgs, namedLabel, execStart)
	if err != nil {
		return err
	}

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
	var execErr error
	if managedLaunchNonce != "" {
		execErr = reexecManagedLaunchWrapper(root, agentHandle, managedLaunchNonce, binaryPath, argv, env, executionOptions)
	} else {
		execErr = coopExecProcess(binaryPath, argv, env)
	}
	if execErr == nil {
		execErr = fmt.Errorf("exec returned without replacing process")
	}
	return cleanupAfterError(execErr)
}

// reexecManagedLaunchWrapper preserves the coop exec PID while moving the
// final trust revalidation and conversation acknowledgement directly next to
// the provider exec boundary.
func reexecManagedLaunchWrapper(root, handle, nonce, binaryPath string, argv, env []string, options *launch.PrepareExecutionOptions) error {
	current, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current amq executable for managed launch: %w", err)
	}
	amqPath, err := coopWakeExecutionPath(current)
	if err != nil {
		return err
	}
	wrapperArgv := []string{
		amqPath, "__launch-exec", "--root", root, "--handle", handle,
		"--nonce", nonce, "--target", binaryPath,
	}
	if options != nil {
		encoded, err := encodeManagedExecutionOptions(*options)
		if err != nil {
			return fmt.Errorf("encode managed execution options: %w", err)
		}
		wrapperArgv = append(wrapperArgv, "--"+managedExecutionOptionsFlag, encoded)
	}
	wrapperArgv = append(wrapperArgv, "--")
	wrapperArgv = append(wrapperArgv, argv...)
	return coopExecProcess(amqPath, wrapperArgv, env)
}

type managedExecutionGuard struct {
	root    *fsq.DeliveryRoot
	handle  string
	nonce   string
	options *launch.PrepareExecutionOptions
	closed  bool
}

func acquireManagedExecutionGuard(rootPath, handle, nonce string, options *launch.PrepareExecutionOptions) (*managedExecutionGuard, error) {
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open existing ticket-bound root: %w", err)
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		return nil, err
	}
	if err := fsq.ValidateExistingMailboxLayout(root, handle); err != nil {
		_ = root.Close()
		return nil, err
	}
	guard := &managedExecutionGuard{root: root, handle: handle, nonce: nonce, options: options}
	if err := guard.Revalidate(); err != nil {
		return nil, errors.Join(err, guard.Close())
	}
	return guard, nil
}

func (guard *managedExecutionGuard) Revalidate() error {
	if guard == nil || guard.closed || guard.root == nil {
		return fmt.Errorf("managed execution guard is not active")
	}
	return launch.ValidateExecutionOptions(guard.root, guard.handle, guard.nonce, guard.options)
}

func (guard *managedExecutionGuard) Close() error {
	if guard == nil || guard.closed {
		return nil
	}
	guard.closed = true
	return guard.root.Close()
}

// coopProvisionAfterClassifyHook runs between layout classification and the
// provisioning writes. Test-only seam for the alias-swap boundary proof.
var coopProvisionAfterClassifyHook func()

// provisionSelfMailboxInExistingRoot provisions one agent mailbox inside a
// pre-existing root, refusing trees that are not queues. The pinned
// capability opened for classification is retained through the writes, so the
// lexical root cannot be aliased between validation and provisioning.
func provisionSelfMailboxInExistingRoot(root, agentHandle string) error {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return fmt.Errorf("inspect root %q: %w", root, err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return fmt.Errorf("open root %q: %w", root, err)
	}
	defer func() { _ = deliveryRoot.Close() }()

	state, err := deliveryRoot.ClassifyLayout()
	if err != nil {
		return fmt.Errorf("refusing to provision in %q: %w", root, err)
	}
	if coopProvisionAfterClassifyHook != nil {
		coopProvisionAfterClassifyHook()
	}
	switch state {
	case fsq.LayoutForeign:
		return NotFoundError(
			"root %q exists but is not an initialized AMQ queue root (no agents/ directory); "+
				"point --root at an existing queue, or create one with: amq init --root %s --agents %s",
			root, shellQuoteArg(root), shellQuoteArg(agentHandle),
		)
	case fsq.LayoutEmpty, fsq.LayoutInitialized:
		// Empty pre-made directories provision the same full tree a missing
		// root gets today; initialized roots idempotently gain any missing
		// top-level directories so a partial tree converges to a valid one.
		if err := deliveryRoot.EnsureRootDirs(); err != nil {
			return fmt.Errorf("failed to ensure root layout at %q: %w", root, err)
		}
	}
	if err := deliveryRoot.EnsureAgentDirs(agentHandle); err != nil {
		return fmt.Errorf("failed to ensure mailbox for %s: %w", agentHandle, err)
	}
	return nil
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

type retainedCoopWakeHelperClaim struct {
	inspection wakeLockInspection
	agentDir   *wakeAgentDir
}

func (claim *retainedCoopWakeHelperClaim) Close() error {
	if claim == nil || claim.agentDir == nil {
		return nil
	}
	err := claim.agentDir.Close()
	claim.agentDir = nil
	return err
}

func captureRetainedCoopWakeHelperClaim(
	proc *os.Process,
	root, me string,
) (*retainedCoopWakeHelperClaim, error) {
	agentDir, err := openExistingCoopWakeAgentDir(root, me)
	if err != nil || agentDir == nil {
		return nil, err
	}
	var current wakeLockInspection
	if err := agentDir.withFD(func(dirfd int) error {
		current = inspectWakeLockAt(dirfd, agentDir, root, me)
		return nil
	}); err != nil {
		return nil, errors.Join(err, agentDir.Close())
	}
	retained := exactCoopWakeHelperClaim(proc, current)
	if retained == nil {
		return nil, agentDir.Close()
	}
	return &retainedCoopWakeHelperClaim{inspection: *retained, agentDir: agentDir}, nil
}

var beforeValidatePreparedCoopWakeClaim = func(*wakeAgentDir) {}

func validatePreparedCoopWakeClaim(
	root string,
	me string,
	injectVia string,
	injectArgs []string,
	owner wakeOwner,
	helperPID int,
) (wakeLockInspection, error) {
	agentDir, err := openExistingCoopWakeAgentDir(root, me)
	if err != nil {
		return wakeLockInspection{}, err
	}
	if agentDir == nil {
		return wakeLockInspection{}, fmt.Errorf("prepared coop wake agent directory is missing")
	}
	defer func() { _ = agentDir.Close() }()
	beforeValidatePreparedCoopWakeClaim(agentDir)

	var current wakeLockInspection
	err = withExistingWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
			return err
		}
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
		target, err := validateAuthoritativeWakeClaimPairAt(scope, current)
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
	helperClaim *retainedCoopWakeHelperClaim,
) error {
	if helperClaim != nil {
		defer func() { _ = helperClaim.Close() }()
	}
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
		"--refuse-unverified-wake",
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
	agentDir, err := openExistingCoopWakeAgentDir(root, me)
	if err != nil {
		_ = killWakeHelperProcess(proc)
		if waitErr := waiter.waitForExit(wakeProcessExitTimeout); waitErr != nil {
			return errors.Join(err, waitErr)
		}
		return err
	}
	if agentDir == nil {
		_ = killWakeHelperProcess(proc)
		if err := waiter.waitForExit(wakeProcessExitTimeout); err != nil {
			return err
		}
		// No directory capability existed before termination. A directory and
		// lock published afterward may belong to a successor, so preserve it for
		// the ordinary stale-lock recovery path instead of reopening by pathname.
		return nil
	}
	defer func() { _ = agentDir.Close() }()
	var expected wakeLockInspection
	if err := agentDir.withFD(func(dirfd int) error {
		expected = inspectWakeLockAt(dirfd, agentDir, root, me)
		return nil
	}); err != nil {
		_ = killWakeHelperProcess(proc)
		if waitErr := waiter.waitForExit(wakeProcessExitTimeout); waitErr != nil {
			return errors.Join(err, waitErr)
		}
		return err
	}
	return terminateWakeHelperProcessInDir(proc, waiter, root, me, agentDir, expected)
}

func terminateWakeHelperProcessInDir(
	proc *os.Process,
	waiter *wakeProcessWaiter,
	root, me string,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
) error {
	if proc == nil || waiter == nil || agentDir == nil {
		return fmt.Errorf("retained wake helper cleanup capability is incomplete")
	}
	ownedGeneration := confirmedLiveWake(expected) && expected.PID == proc.Pid && expected.Lock.Generation != ""
	_ = killWakeHelperProcess(proc)
	if err := waiter.waitForExit(wakeProcessExitTimeout); err != nil {
		return err
	}
	if ownedGeneration {
		return cleanupTerminatedWakeLockInDir(agentDir, expected)
	}
	return cleanupTerminatedWakeLockForPIDInDir(agentDir, root, me, proc.Pid)
}

func cleanupStartedWakeHelper(
	proc *os.Process,
	waiter *wakeProcessWaiter,
	capability *authoritativeWakeChildCapability,
	owner *wakeOwner,
	root string,
	me string,
	helperClaim *retainedCoopWakeHelperClaim,
) error {
	if owner == nil {
		if helperClaim != nil {
			return terminateWakeHelperProcessInDir(
				proc,
				waiter,
				root,
				me,
				helperClaim.agentDir,
				helperClaim.inspection,
			)
		}
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
	claim, err := captureRetainedCoopWakeHelperClaim(proc, root, me)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("retained wake helper claim is missing")
	}
	defer func() { _ = claim.Close() }()
	return terminateAuthoritativeWakeHelperProcessForClaim(
		proc,
		waiter,
		capability,
		root,
		me,
		owner,
		claim,
	)
}

func terminateAuthoritativeWakeHelperProcessForClaim(
	proc *os.Process,
	waiter *wakeProcessWaiter,
	capability *authoritativeWakeChildCapability,
	root string,
	me string,
	owner wakeOwner,
	helperClaim *retainedCoopWakeHelperClaim,
) error {
	if capability == nil {
		return fmt.Errorf("stable owner-bound wake child capability is missing")
	}
	var claimErr error
	if helperClaim != nil {
		if helperClaim.agentDir == nil {
			claimErr = fmt.Errorf("retained helper agent directory is missing")
		} else {
			claimErr = helperClaim.agentDir.withFD(func(dirfd int) error {
				current := inspectWakeLockAt(dirfd, helperClaim.agentDir, root, me)
				if !sameWakeLockGeneration(helperClaim.inspection, current) {
					return fmt.Errorf("generic helper wake claim changed before termination")
				}
				return nil
			})
		}
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

	if waitErr == nil && helperClaim != nil && claimErr == nil {
		switch classifyPersistedWakeClaim(helperClaim.inspection) {
		case wakeClaimAuthoritative:
			claimErr = rollbackAuthoritativeWakeClaimInDir(helperClaim.agentDir, root, me, owner, helperClaim.inspection)
		case wakeClaimGeneric:
			claimErr = cleanupTerminatedWakeLockInDir(helperClaim.agentDir, helperClaim.inspection)
		case wakeClaimAbsent:
		default:
			claimErr = fmt.Errorf("retained helper wake claim is unverified; preserving it")
		}
	}
	return errors.Join(stopErr, waitErr, closeErr, claimErr)
}

func rollbackAuthoritativeWakeClaim(root, me string, owner wakeOwner) error {
	agentDir, err := openExistingCoopWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	if agentDir == nil {
		return fmt.Errorf("retained authoritative wake agent directory is missing")
	}
	defer func() { _ = agentDir.Close() }()
	var expected wakeLockInspection
	if err := agentDir.withFD(func(dirfd int) error {
		expected = inspectWakeLockAt(dirfd, agentDir, root, me)
		return nil
	}); err != nil {
		return err
	}
	return rollbackAuthoritativeWakeClaimInDir(agentDir, root, me, owner, expected)
}

func rollbackAuthoritativeWakeClaimInDir(
	agentDir *wakeAgentDir,
	root, me string,
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
	return withExistingWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
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
		relation, relationErr := retainedWakeAgentDirRelationAt(agentDir, dirfd)
		if relationErr != nil {
			return newWakeStateBoundInconclusiveError(relationErr)
		}
		switch relation {
		case wakeAgentDirCanonical:
		case wakeAgentDirDetached:
			if current.Status != wakeLockStale {
				return fmt.Errorf("owner-bound wake is not conclusively absent after helper stop")
			}
			return removeAuthoritativeWakeClaimAt(scope, current, nil)
		case wakeAgentDirInconclusive:
			return newWakeStateBoundInconclusiveError(
				fmt.Errorf("wake agent directory relation is inconclusive after helper stop"),
			)
		default:
			return newWakeStateBoundInconclusiveError(
				fmt.Errorf("unknown wake agent directory relation %d", relation),
			)
		}
		target, err := validateAuthoritativeWakeClaimPairAt(scope, current)
		if err != nil {
			return err
		}
		if current.Status != wakeLockStale {
			return fmt.Errorf("owner-bound wake is not conclusively absent after helper stop")
		}
		return removeAuthoritativeWakeClaimAt(scope, current, &target)
	})
}

func cleanupTerminatedWakeLock(expected wakeLockInspection) error {
	agentDir, err := openExistingCoopWakeAgentDir(expected.Root, expected.Agent)
	if err != nil || agentDir == nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return cleanupTerminatedWakeLockInDir(agentDir, expected)
}

func cleanupTerminatedWakeLockInDir(agentDir *wakeAgentDir, expected wakeLockInspection) error {
	return withExistingWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
		if !sameWakeLockGeneration(expected, current) {
			return nil
		}
		if current.Status != wakeLockStale {
			return fmt.Errorf("terminated wake lock is not proven stale: %s", current.Status)
		}
		if err := validateWakeLockStaleRemovalAtForTermination(dirfd, agentDir, current); err != nil {
			return err
		}
		return removeWakeLockIfUnchangedGuardedAt(scope, current)
	})
}

func cleanupTerminatedWakeLockForPIDInDir(
	agentDir *wakeAgentDir,
	root, me string,
	terminatedPID int,
) error {
	return withExistingWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !current.Exists || current.PID != terminatedPID || current.Lock.Generation == "" {
			return nil
		}
		if current.Status != wakeLockStale {
			return fmt.Errorf("terminated wake lock is not proven stale: %s", current.Status)
		}
		if err := validateWakeLockStaleRemovalAtForTermination(dirfd, agentDir, current); err != nil {
			return err
		}
		return removeWakeLockIfUnchangedGuardedAt(scope, current)
	})
}

func wakeReadyMessage(root, agentHandle string, startedPID int) string {
	if inspection := inspectWakeLock(root, agentHandle); inspection.Status == wakeLockValid && inspection.PID != 0 && inspection.PID != startedPID {
		return fmt.Sprintf("Using existing amq wake (pid %d)", inspection.PID)
	}
	return fmt.Sprintf("Started amq wake (pid %d)", startedPID)
}

// resolveCoopAmbientSessionBase returns the base selected by an inherited AMQ
// context. A complete pin owns routing; otherwise AM_ROOT keeps its normal
// precedence and is classified as either a base or a session root. Callers use
// the project/global fallback only when no ambient context exists.
func resolveCoopAmbientSessionBase() (string, bool, error) {
	pin, err := loadSessionPin()
	if err != nil {
		return "", false, err
	}
	if pin.Present {
		if err := validateLegacySessionPinRoot(pin); err != nil {
			return "", false, err
		}
		if pin.IdentityPin {
			ambientRoot := strings.TrimSpace(os.Getenv(envRoot))
			if ambientRoot == "" {
				ambientRoot = pin.ExpectedRoot
			}
			if err := verifyRootUnderBase(
				pin.BaseRoot,
				pin.BaseRootID,
				pin.Session,
				ambientRoot,
				pin.RootID,
			); err != nil {
				return "", false, err
			}
		}
		return pin.BaseRoot, true, nil
	}

	ambientRoot := strings.TrimSpace(os.Getenv(envRoot))
	if ambientRoot == "" {
		return "", false, nil
	}
	return absPath(baseRootOf(ambientRoot)), true, nil
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
