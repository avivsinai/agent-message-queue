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

var coopExecProcess = syscall.Exec

// startCoopWake starts the coop wake helper process.
var startCoopWake = (*exec.Cmd).Start

func openCoopWakeTTY() (*os.File, error) {
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

// coopExecRun is the state of one coop exec invocation, shared by its phases.
type coopExecRun struct {
	managedLaunchNonce           string
	fs                           *flag.FlagSet
	rootFlag                     *string
	sessionFlag                  *string
	meFlag                       *string
	noInitFlag                   *bool
	noGitignoreFlag              *bool
	noWakeFlag                   *bool
	requireWakeFlag              *bool
	wakeInjectModeFlag           *string
	wakeInjectViaFlag            *string
	wakeInjectArgFlags           multiStringFlag
	managedNoWakeReasonFlag      *string
	managedSymphonyEventFlags    multiStringFlag
	managedSymphonyWorkspaceFlag *string
	yesFlag                      *bool
	namedFlag                    *bool
	agentArgs                    []string
	named                        coopNamedChoice
	cmdName                      string
	wakeInjectVia                string
	wakeInjectMode               string
	executionOptions             *launch.PrepareExecutionOptions
	agentHandle                  string
	managedGuard                 *managedExecutionGuard
	rootRequested                bool
	selectorFree                 bool
	defaultSession               string
	warnedCreation               bool
	bootstrapBase                string
	root                         string
	sessionProvisioned           bool
	binaryPath                   string
	creationDeprecated           bool
	baseEnv                      []string
	earlyOwner                   *wakeOwner
	wakeProc                     *os.Process
	wakeWaiter                   *wakeProcessWaiter
	wakeChildCapability          *authoritativeWakeChildCapability
	wakeHelperClaim              *retainedCoopWakeHelperClaim
	wakeHelperClaimErr           error
	cleanupWakeReady             func()
	retainWakeHelperClaim        func()
	cleanupWakeHelper            func(preservePersistedClaim bool) error
	// wakeOwned is true once this launch's wake is ready and its claim is
	// validated as this process's.
	wakeOwned bool
}

func runCoopExec(args []string) error {
	c := &coopExecRun{}
	amqArgs, usage := c.defineFlags(args)

	if handled, err := parseFlagsAllowPositionals(c.fs, amqArgs, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	var err error
	if err := c.validateFlags(); err != nil {
		return err
	}

	// Derive agent handle from command basename (or --me override).
	c.agentHandle = *c.meFlag
	if c.agentHandle == "" {
		c.agentHandle = strings.ToLower(filepath.Base(c.cmdName))
	}
	c.agentHandle, err = normalizeHandle(c.agentHandle)
	if err != nil {
		return fmt.Errorf("cannot derive agent handle from %q: %w (use --me to override)", c.cmdName, err)
	}
	if c.managedLaunchNonce != "" {
		c.managedGuard, err = acquireManagedExecutionGuard(*c.rootFlag, c.agentHandle, c.managedLaunchNonce, c.executionOptions)
		if err != nil {
			return ActionRequiredError("refusing managed execution options before root mutation: %v", err)
		}
		defer func() { _ = c.managedGuard.Close() }()
	}

	if err := c.selectRoot(); err != nil {
		return err
	}
	if err := c.provisionRoot(); err != nil {
		return err
	}
	if !*c.noWakeFlag {
		if err := prepareCoopWakeLock(
			c.root,
			c.agentHandle,
			*c.yesFlag,
			coopWakeRemedyForCommand(c.root, c.agentHandle, c.cmdName, c.agentArgs),
		); err != nil {
			return err
		}
	}

	// Start amq wake in background (unless --no-wake). Every coop-started wake
	// is bound to this exact process identity and exits when the exec-replaced
	// agent exits. On failed Exec, stable child cleanup stops it immediately.
	c.baseEnv = unsetEnvVar(
		unsetEnvVar(
			unsetEnvVar(
				unsetEnvVar(os.Environ(), launch.InternalLaunchNonceEnv),
				envWakeOwner,
			),
			envWakePrivateStopFD,
		),
		envWakeAttentionFD,
	)
	c.retainWakeHelperClaim = func() {
		retained, err := captureRetainedCoopWakeHelperClaim(c.wakeProc, c.root, c.agentHandle)
		if err != nil {
			c.wakeHelperClaimErr = errors.Join(c.wakeHelperClaimErr, err)
			return
		}
		if retained != nil {
			if c.wakeHelperClaim != nil {
				_ = c.wakeHelperClaim.Close()
			}
			c.wakeHelperClaim = retained
		}
	}
	c.cleanupWakeHelper = func(preservePersistedClaim bool) error {
		cleanupErr := cleanupCoopWakeStartupHelper(
			c.wakeProc,
			c.wakeWaiter,
			c.wakeChildCapability,
			c.earlyOwner,
			c.root,
			c.agentHandle,
			preservePersistedClaim,
			c.wakeHelperClaim,
		)
		c.wakeHelperClaim = nil
		return errors.Join(cleanupErr, c.wakeHelperClaimErr)
	}
	if !*c.noWakeFlag {
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
		c.earlyOwner = &captured
		// Before the wake can admit any input: a doorbell typed before the
		// harness starts waits in the terminal and answers its trust dialog.
		// prepareCoopWakeLock has refused a live other owner; a launch that
		// then loses the race leaves a marker that only holds.
		writeStartupDialogMarker(c.root, c.agentHandle, c.binaryPath, c.agentArgs)
		readyPath, cleanupReady, readyErr := newWakeReadyFile()
		if readyErr != nil {
			inspection := inspectWakeLock(c.root, c.agentHandle)
			if err := handleCoopWakeSetupFailure(*c.requireWakeFlag, inspection, "create wake readiness file", readyErr); err != nil {
				return err
			}
		} else {
			c.cleanupWakeReady = cleanupReady
			defer cleanupReady()
			wakeCmd := exec.Command(amqBin, buildCoopWakeArgs(c.agentHandle, c.root, c.wakeInjectMode, c.wakeInjectVia, []string(c.wakeInjectArgFlags), readyPath)...)
			if wakeLaunchLocator != "" {
				// Execute the already-running image, but preserve the absolute,
				// unresolved invocation locator as argv[0] so the child can safely
				// validate and observe installer symlink flips without a launch TOCTOU.
				wakeCmd.Args[0] = wakeLaunchLocator
			}
			// Set AM_ROOT in wake's env so the helper process resolves the same
			// session root even if the parent shell inherited a different value.
			wakeEnv, wakeEnvErr := wakeCommandEnv(c.baseEnv, c.root, c.earlyOwner)
			if wakeEnvErr != nil {
				return wakeEnvErr
			}
			wakeCmd.Env = wakeEnv
			wakeCmd.Stdin = os.Stdin
			wakeOutput, outputErr := openCoopWakeOutput(c.root, c.agentHandle)
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
			if err := c.superviseWakeHelper(wakeCmd, readyPath); err != nil {
				return err
			}
		}
	}
	if c.managedGuard != nil {
		if err := c.managedGuard.Close(); err != nil {
			return ActionRequiredError("release managed execution option guard after wake startup: %v", err)
		}
	}
	// Removing a leftover Startup marker could shorten another launch's
	// hold, so only a launch that owns the agent's wake does it.
	if c.wakeOwned {
		clearStaleStartupDialogMarker(c.root, c.agentHandle, c.binaryPath, c.agentArgs)
	}

	return c.execAgent()
}

// defineFlags registers the coop exec flags on c.fs, splitting the
// agent's own flags at "--". It returns the AMQ arguments and the usage.
func (c *coopExecRun) defineFlags(args []string) ([]string, func()) {
	c.managedLaunchNonce = strings.TrimSpace(os.Getenv(launch.InternalLaunchNonceEnv))
	// Split at "--" before flag parsing so agent flags aren't consumed.
	var amqArgs []string
	amqArgs, c.agentArgs = splitDashDash(args)

	c.fs = flag.NewFlagSet("coop exec", flag.ContinueOnError)
	c.rootFlag = c.fs.String("root", "", "Root directory (override auto-detection)")
	c.sessionFlag = c.fs.String("session", "", "Session name (shorthand for --root .agent-mail/<name>)")
	c.meFlag = c.fs.String("me", "", "Agent handle (override auto-derivation from command name)")
	c.noInitFlag = c.fs.Bool("no-init", false, "Don't auto-initialize if .amqrc is missing")
	c.noGitignoreFlag = c.fs.Bool("no-gitignore", false, "When auto-initializing, do not modify .gitignore")
	c.noWakeFlag = c.fs.Bool("no-wake", false, "Don't start amq wake in background")
	c.requireWakeFlag = c.fs.Bool("require-wake", false, "Fail if amq wake cannot start and acquire its lock")
	c.wakeInjectModeFlag = c.fs.String("wake-inject-mode", wakeInjectModeAuto, "Wake injection mode: auto, raw, paste, none")
	c.wakeInjectViaFlag = c.fs.String("wake-inject-via", "", "Start wake with this absolute --inject-via executable, enabling later amq wake repair")
	c.fs.Var(&c.wakeInjectArgFlags, "wake-inject-arg", "Fixed argument for wake --inject-via before the payload (repeatable)")
	c.managedNoWakeReasonFlag = c.fs.String("managed-no-wake-reason", "", "")
	c.fs.Var(&c.managedSymphonyEventFlags, "managed-symphony-event", "")
	c.managedSymphonyWorkspaceFlag = c.fs.String("managed-symphony-workspace-key", "", "")
	c.yesFlag = c.fs.Bool("y", false, "Skip confirmation prompts (including clearing a blocking wake)")
	c.namedFlag = c.fs.Bool("named", true, "Stamp the session and AM_ME onto the spawned CLI session name")

	usage := usageWithFlags(c.fs, "amq coop exec [options] <command> [-- <command-flags>]",
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
		"  amq coop exec agy                                 # Antigravity CLI, caller flags forwarded as-is",
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
	return amqArgs, usage
}

// validateFlags checks the parsed flags and derives the command, the
// session naming choice, the wake injector and managed execution options.
func (c *coopExecRun) validateFlags() error {
	var err error
	c.named, err = resolveCoopNamedEnabled(flagWasVisited(c.fs, "named"), *c.namedFlag)
	if err != nil {
		return err
	}
	if c.managedLaunchNonce != "" {
		// Managed launches use the ticket's exact provider argv for naming.
		c.named.enabled = false
	}
	remaining := c.fs.Args()
	if len(remaining) == 0 {
		return UsageError("command required (e.g., 'claude', 'codex', 'bash')")
	}
	c.cmdName = remaining[0]
	// Extra positional args before "--" are appended to agent args.
	if len(remaining) > 1 {
		c.agentArgs = append(remaining[1:], c.agentArgs...)
	}
	// Before any root, wake or naming work. A managed plan's arguments
	// passed the launch grammar, which admits no --remote and no identity
	// override.
	if c.managedLaunchNonce == "" {
		if err := refuseCodexSeatArgs(c.cmdName, c.agentArgs); err != nil {
			return err
		}
	}
	if err := refuseCodexIncludePolicy(c.cmdName, c.agentArgs); err != nil {
		return err
	}
	if c.managedLaunchNonce != "" && flagWasVisited(c.fs, "named") && *c.namedFlag && !launch.ArgsHaveNameFlag(c.agentArgs) {
		return UsageError("--named is not supported under a managed launch; declare the name in the launch plan instead")
	}
	if c.managedLaunchNonce != "" {
		if !flagWasVisited(c.fs, "root") || strings.TrimSpace(*c.rootFlag) == "" || !filepath.IsAbs(*c.rootFlag) {
			return ActionRequiredError("managed launch requires one explicit absolute --root")
		}
		if *c.sessionFlag != "" {
			return ActionRequiredError("managed launch forbids --session; use the ticket-bound exact --root")
		}
	}
	if *c.noWakeFlag && *c.requireWakeFlag {
		return UsageError("--require-wake cannot be used with --no-wake")
	}
	declaredWakeInjectVia := strings.TrimSpace(*c.wakeInjectViaFlag)
	c.wakeInjectVia = declaredWakeInjectVia
	c.wakeInjectMode, err = normalizeWakeInjectMode(*c.wakeInjectModeFlag)
	if err != nil {
		return UsageError("--wake-inject-mode: %v", err)
	}
	if *c.wakeInjectViaFlag != "" && c.wakeInjectVia == "" {
		return UsageError("--wake-inject-via must not be blank")
	}
	if c.wakeInjectMode == wakeInjectModeNone && c.wakeInjectVia != "" {
		return UsageError("--wake-inject-via cannot be used with --wake-inject-mode none")
	}
	if c.wakeInjectMode == wakeInjectModeNone && len(c.wakeInjectArgFlags) > 0 {
		return UsageError("--wake-inject-arg cannot be used with --wake-inject-mode none")
	}
	if c.wakeInjectVia == "" && len(c.wakeInjectArgFlags) > 0 {
		return UsageError("--wake-inject-arg requires --wake-inject-via")
	}
	if c.wakeInjectVia != "" {
		resolvedWakeInjectVia, err := validateWakeInjectViaPath(c.wakeInjectVia)
		if err != nil {
			return UsageError("--wake-inject-via: %v", err)
		}
		c.wakeInjectVia = resolvedWakeInjectVia
	}
	managedOnlyOptions := flagWasVisited(c.fs, "managed-no-wake-reason") ||
		flagWasVisited(c.fs, "managed-symphony-event") ||
		flagWasVisited(c.fs, "managed-symphony-workspace-key")
	if managedOnlyOptions && c.managedLaunchNonce == "" {
		return UsageError("managed execution options require a trusted launch ticket")
	}
	if c.managedLaunchNonce != "" && (managedOnlyOptions || flagWasVisited(c.fs, "named") ||
		flagWasVisited(c.fs, "no-gitignore") || flagWasVisited(c.fs, "no-wake") ||
		flagWasVisited(c.fs, "require-wake") || flagWasVisited(c.fs, "wake-inject-mode") ||
		flagWasVisited(c.fs, "wake-inject-via") || flagWasVisited(c.fs, "wake-inject-arg")) {
		wakeMode := "enabled"
		if *c.noWakeFlag {
			wakeMode = "disabled"
		}
		managedInjectorMode := ""
		if flagWasVisited(c.fs, "wake-inject-mode") {
			managedInjectorMode = c.wakeInjectMode
		}
		options := launch.PrepareExecutionOptions{
			RequireWake:          *c.requireWakeFlag,
			NoGitignore:          *c.noGitignoreFlag,
			Named:                flagWasVisited(c.fs, "named") && *c.namedFlag,
			WakeMode:             wakeMode,
			AuditReason:          *c.managedNoWakeReasonFlag,
			InjectorMode:         managedInjectorMode,
			InjectorVia:          declaredWakeInjectVia,
			InjectorArgs:         append([]string(nil), c.wakeInjectArgFlags...),
			SymphonyEvents:       append([]string(nil), c.managedSymphonyEventFlags...),
			SymphonyWorkspaceKey: *c.managedSymphonyWorkspaceFlag,
		}
		if err := validateManagedExecutionOptions(options); err != nil {
			return UsageError("managed execution options: %v", err)
		}
		c.executionOptions = &options
	}
	return nil
}

// selectRoot resolves the session selector and the root, finds the agent
// command, and reads the declared default session, all before the first
// provisioning mutation.
func (c *coopExecRun) selectRoot() error {
	var err error
	c.rootRequested = flagWasVisited(c.fs, "root")
	sessionRequested := *c.sessionFlag != ""
	c.selectorFree = !c.rootRequested && !sessionRequested
	c.defaultSession = defaultSessionName

	// Resolve explicit --session (pure sugar for --root <base>/<session>).
	// A fresh Git worktree has no base yet; remember that bootstrap is needed
	// and provision the requested session after full coop init completes.
	c.bootstrapBase = ""
	if *c.sessionFlag != "" {
		if flagWasVisited(c.fs, "root") {
			return UsageError("--session and --root are mutually exclusive")
		}
		if err := validateSessionName(*c.sessionFlag); err != nil {
			return err
		}
		base, ambient, err := resolveCoopAmbientSessionBase()
		if err != nil {
			return err
		}
		if !ambient {
			if *c.noInitFlag {
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
					c.bootstrapBase = base
					base = ""
				}
			}
		}
		if base != "" {
			*c.rootFlag = filepath.Join(base, *c.sessionFlag)
		}
	}

	// Resolve root: --root flag (or --session-derived) > ambient context >
	// project/repo-local/global discovery > local initialization.
	c.root = *c.rootFlag
	c.sessionProvisioned = false
	if c.root == "" {
		ambientBase, ambient, ambientErr := resolveCoopAmbientSessionBase()
		if ambientErr != nil {
			return ambientErr
		}
		if ambient {
			c.root = ambientBase
		} else {
			var discoveredBase string
			var found bool
			var discoveryErr error
			if *c.noInitFlag {
				discoveredBase, found, discoveryErr = resolveDiscoveredBaseRoot()
			} else {
				discoveredBase, found, discoveryErr = resolveDiscoveredBaseRootForBootstrap()
			}
			if discoveryErr != nil {
				return discoveryErr
			}
			if found {
				c.root = discoveredBase
			} else if discoveredBase != "" {
				c.bootstrapBase = discoveredBase
			}
		}
	}

	// Resolve the command binary after every pure flag/name validation but
	// before the first provisioning mutation: a typo'd command must fail
	// without minting roots, sessions, or mailboxes, while usage errors keep
	// their established precedence over command-not-found.
	c.binaryPath, err = exec.LookPath(c.cmdName)
	if err != nil {
		return fmt.Errorf("command not found: %s", c.cmdName)
	}

	if c.selectorFree {
		c.defaultSession, err = declaredCoopExecSession()
		if err != nil {
			return err
		}
	}
	c.creationDeprecated, err = hasBootstrapSuppressingDeclaration()
	if err != nil {
		return err
	}
	return nil
}

// provisionRoot creates the session or root when needed, pins the root to
// an absolute path, and ensures the agent mailbox inside a proven queue.
func (c *coopExecRun) provisionRoot() error {
	var err error
	// Explicit named sessions use the same direct-child creation boundary as
	// coop init/default exec. In particular, never let MkdirAll traverse a
	// pre-existing session symlink.
	if *c.sessionFlag != "" && c.root != "" {
		if *c.noInitFlag && !dirExists(c.root) {
			return fmt.Errorf("root %q does not exist; run 'amq coop init' first or remove --no-init", c.root)
		}
		base := filepath.Dir(c.root)
		if !dirExists(base) {
			if err := fsq.EnsureRootDirs(base); err != nil {
				return fmt.Errorf("failed to create base root %q: %w", base, err)
			}
		}
		requestedRoot := c.root
		if !dirExists(requestedRoot) {
			warnCoopExecCreationDeprecated(&c.warnedCreation)
		}
		c.root, err = provisionCoopSession(base, *c.sessionFlag, []string{c.agentHandle}, c.agentHandle, c.cmdName)
		if err != nil {
			return fmt.Errorf("failed to create session root %q: %w", requestedRoot, err)
		}
		c.sessionProvisioned = true
	}

	// Auto-init if needed (before session defaulting so full init fires on fresh projects).
	if c.root == "" || !dirExists(c.root) {
		if *c.noInitFlag {
			if c.root == "" {
				return fmt.Errorf("no project, repo-local, or global AMQ root found and no --root specified; run 'amq coop init' first or remove --no-init")
			}
			return fmt.Errorf("root %q does not exist; run 'amq coop init' first or remove --no-init", c.root)
		}

		if c.root != "" {
			// We have a root (from --root, --session, or .amqrc) — create root + agent dirs.
			if c.rootRequested {
				warnCoopExecCreationDeprecated(&c.warnedCreation)
			}
			delivery, err := fsq.CreateDirectoryPathExclusive(c.root)
			if err != nil {
				return fmt.Errorf("failed to create root %q: %w", c.root, err)
			}
			if err := delivery.EnsureRootDirs(); err != nil {
				_ = delivery.Close()
				return fmt.Errorf("failed to create root %q: %w", c.root, err)
			}
			if err := delivery.EnsureAgentDirs(c.agentHandle); err != nil {
				_ = delivery.Close()
				return fmt.Errorf("failed to create mailbox for %s at %q: %w", c.agentHandle, c.root, err)
			}
			_ = delivery.Close()
		} else {
			// No explicit, ambient, project, repo-local, or global root: run
			// full coop init. In a Git worktree, coop init relocates this to
			// the worktree top so nested invocations do not scatter queues.
			if !*c.yesFlag {
				location := "current directory"
				if c.bootstrapBase != "" {
					location = fmt.Sprintf("Git worktree at %s", filepath.Dir(c.bootstrapBase))
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
			if *c.noGitignoreFlag {
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
			c.root = existing.Config.Root
			if c.root != "" && !filepath.IsAbs(c.root) {
				c.root = filepath.Join(existing.Dir, c.root)
			}
		}
	}

	if *c.sessionFlag != "" && !c.sessionProvisioned {
		base := c.root
		if c.bootstrapBase != "" && !sameTreeIdentity(base, c.bootstrapBase) {
			return fmt.Errorf("bootstrap resolved base %q, but coop init produced %q", c.bootstrapBase, base)
		}
		requestedRoot := filepath.Join(base, *c.sessionFlag)
		if !dirExists(requestedRoot) {
			warnCoopExecCreationDeprecated(&c.warnedCreation)
		}
		c.root, err = provisionCoopSession(base, *c.sessionFlag, []string{c.agentHandle}, c.agentHandle, c.cmdName)
		if err != nil {
			return fmt.Errorf("failed to create session root %q: %w", requestedRoot, err)
		}
		c.sessionProvisioned = true
	}

	// Default to the declared session (or collab) when neither --session nor
	// --root was specified. This runs after auto-init so .amqrc exists.
	if c.selectorFree {
		base := c.root // root is the literal .amqrc root (e.g., .agent-mail)
		requestedRoot := filepath.Join(base, c.defaultSession)
		if c.creationDeprecated && !dirExists(requestedRoot) {
			warnCoopExecCreationDeprecated(&c.warnedCreation)
		}
		c.root, err = provisionCoopSession(base, c.defaultSession, []string{c.agentHandle}, c.agentHandle, c.cmdName)
		if err != nil {
			return fmt.Errorf("failed to create session root %q: %w", requestedRoot, err)
		}
		c.sessionProvisioned = true
	}

	// Pin the session root to an absolute path before it reaches the wake
	// process and the exported AM_ROOT/AM_BASE_ROOT. A relative root
	// re-resolves against every future cwd of the agent process, silently
	// splitting one session name into per-directory mailbox trees
	// (messages land where no peer is reading).
	c.root, err = absoluteSessionRoot(c.root)
	if err != nil {
		return fmt.Errorf("resolve absolute session root: %w", err)
	}
	// Ensure the agent mailbox exists — but only inside a tree that is
	// provably a queue. A pre-existing directory is not a provisioning
	// target just because it exists (dirExists said nothing about what it
	// is), so classify the layout through a pinned capability first and keep
	// that same capability for the writes.
	if !c.sessionProvisioned {
		if c.managedGuard != nil {
			if err := fsq.ValidateExistingMailboxLayout(c.managedGuard.root, c.agentHandle); err != nil {
				return ActionRequiredError("refusing managed mailbox provisioning: %v", err)
			}
		} else {
			if err := provisionSelfMailboxInExistingRoot(c.root, c.agentHandle); err != nil {
				return err
			}
		}
	}
	return nil
}

// superviseWakeHelper starts the prepared exact-owner wake helper, binds it,
// waits for readiness, and validates or cleans up its claim. A setup failure
// that is not required returns nil with no helper retained.
func (c *coopExecRun) superviseWakeHelper(wakeCmd *exec.Cmd, readyPath string) error {
	var err error
	c.wakeChildCapability, err = configureAuthoritativeWakeChild(wakeCmd)
	if err == nil && c.wakeChildCapability == nil {
		return fmt.Errorf("prepare exact-owner amq wake supervision returned nil capability")
	}
	if err == nil && c.managedGuard != nil {
		if validateErr := c.managedGuard.Revalidate(); validateErr != nil {
			closeErr := c.wakeChildCapability.Close()
			c.wakeChildCapability = nil
			return ActionRequiredError(
				"refusing changed managed execution options before wake start: %v",
				errors.Join(validateErr, closeErr),
			)
		}
	}
	if err != nil {
		var closeErr error
		if c.wakeChildCapability != nil {
			closeErr = c.wakeChildCapability.Close()
			c.wakeChildCapability = nil
		}
		if closeErr != nil {
			return errors.Join(
				fmt.Errorf("prepare exact-owner amq wake supervision: %w", err),
				fmt.Errorf("cleanup unstarted coop wake capability: %w", closeErr),
			)
		}
		inspection := inspectWakeLock(c.root, c.agentHandle)
		if setupErr := handleCoopWakeSetupFailure(
			*c.requireWakeFlag,
			inspection,
			"prepare exact-owner amq wake supervision",
			err,
		); setupErr != nil {
			return setupErr
		}
	} else if err := startCoopWake(wakeCmd); err != nil {
		var closeErr error
		if c.wakeChildCapability != nil {
			closeErr = c.wakeChildCapability.Close()
			c.wakeChildCapability = nil
		}
		if closeErr != nil {
			return errors.Join(
				fmt.Errorf("start exact-owner amq wake helper: %w", err),
				fmt.Errorf("cleanup unstarted coop wake capability: %w", closeErr),
			)
		}
		inspection := inspectWakeLock(c.root, c.agentHandle)
		if err := handleCoopWakeSetupFailure(*c.requireWakeFlag, inspection, "start exact-owner amq wake helper", err); err != nil {
			return err
		}
	} else {
		c.wakeProc = wakeCmd.Process
		c.wakeWaiter = newWakeProcessWaiter(c.wakeProc)
		if c.wakeChildCapability != nil {
			if err := c.wakeChildCapability.Bind(c.wakeProc); err != nil {
				current := inspectWakeLock(c.root, c.agentHandle)
				c.retainWakeHelperClaim()
				cleanupErr := c.cleanupWakeHelper(current.Exists && current.PID != c.wakeProc.Pid)
				bindErr := fmt.Errorf("bind stable owner-bound wake child: %w", err)
				if cleanupErr == nil {
					return bindErr
				}
				return errors.Join(bindErr, fmt.Errorf("cleanup exact coop wake startup helper: %w", cleanupErr))
			}
		}
		readyErr := waitForWakeReadyWithOwner(
			c.wakeWaiter,
			readyPath,
			c.root,
			c.agentHandle,
			c.earlyOwner,
			wakeReadyTimeout,
		)
		current := inspectWakeLock(c.root, c.agentHandle)
		c.retainWakeHelperClaim()
		otherWake := current.Exists && current.PID != c.wakeProc.Pid
		if readyErr != nil {
			startupErr := readyErr
			if otherWake {
				startupErr = coopWakeStartupConflictError(current, readyErr)
			}
			cleanupErr := c.cleanupWakeHelper(otherWake)
			if cleanupErr != nil {
				return errors.Join(
					startupErr,
					fmt.Errorf("cleanup exact coop wake startup helper: %w", cleanupErr),
				)
			}
			if otherWake || *c.requireWakeFlag {
				return startupErr
			}
			_ = writeStderr("warning: failed to prepare amq wake: %v\n", readyErr)
			c.wakeProc = nil
			c.wakeWaiter = nil
			c.wakeChildCapability = nil
		} else {
			current, claimErr := validatePreparedCoopWakeClaim(
				c.root,
				c.agentHandle,
				c.wakeInjectVia,
				[]string(c.wakeInjectArgFlags),
				*c.earlyOwner,
				c.wakeProc.Pid,
			)
			reused := current.Exists && current.PID != c.wakeProc.Pid
			if !reused {
				c.retainWakeHelperClaim()
			}
			if claimErr != nil {
				cleanupErr := c.cleanupWakeHelper(reused)
				if cleanupErr != nil {
					return errors.Join(
						claimErr,
						fmt.Errorf("cleanup exact coop wake startup helper: %w", cleanupErr),
					)
				}
				return claimErr
			}
			if reused {
				if cleanupErr := c.cleanupWakeHelper(true); cleanupErr != nil {
					return fmt.Errorf(
						"finish exact reused-wake startup helper: %w",
						cleanupErr,
					)
				}
				_ = writeStderr("%s\n", wakeReadyMessage(c.root, c.agentHandle, current.PID))
				c.wakeProc = nil
				c.wakeWaiter = nil
				c.wakeChildCapability = nil
			} else {
				_ = writeStderr("%s\n", wakeReadyMessage(c.root, c.agentHandle, c.wakeProc.Pid))
			}
			c.wakeOwned = true
		}
	}
	return nil
}

// execAgent builds the agent environment and argv, applies session naming,
// and replaces the process. It returns only when exec fails, after cleaning
// up the wake helper.
func (c *coopExecRun) execAgent() error {
	var err error
	// A named/default or session-shaped explicit root pins an identity
	// independent of AM_ROOT. A custom sessionless --root clears inherited pins.
	requestedSession := *c.sessionFlag
	if c.selectorFree {
		requestedSession = c.defaultSession
	}
	sessionIdentity := coopSessionIdentity(c.root, requestedSession, *c.rootFlag)
	env := buildCoopExecEnvironment(c.baseEnv, c.root, c.agentHandle, sessionIdentity)

	// Automatic naming uses the resolved session identity. A custom sessionless
	// root has no session prefix and therefore uses only the agent handle.
	namedLabel := coopNamedSessionLabel(sessionIdentity, c.agentHandle)
	execStart := time.Now()
	recordIdentity := codexThreadIdentityRecorder(env)
	c.agentArgs, err = applyCoopNamedBeforeExecAt(c.named, c.binaryPath, c.agentArgs, namedLabel, execStart, recordIdentity)
	if err != nil {
		return err
	}
	c.agentArgs, err = resolveCodexResumeLaunch(c.binaryPath, c.agentArgs)
	if err != nil {
		return err
	}
	recorded, err := recordResumedCodexThread(c.binaryPath, c.agentArgs, recordIdentity)
	if err != nil {
		return err
	}
	// A managed plan carries the identity overrides as its own dynamic
	// slots, which the launch wrapper fills.
	if c.managedLaunchNonce == "" {
		c.agentArgs = pinCodexShellIdentity(c.binaryPath, c.agentArgs, env, recorded)
	}

	// Build argv: command name + agent args.
	argv := append([]string{c.cmdName}, c.agentArgs...)

	// Replace process. On success, this never returns.
	// On failure, clean up the wake process.
	if c.cleanupWakeReady != nil {
		c.cleanupWakeReady()
	}
	cleanupAfterError := func(cause error) error {
		cleanupErr := c.cleanupWakeHelper(false)
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
	if c.earlyOwner != nil && *c.earlyOwner != finalOwner {
		return cleanupAfterError(fmt.Errorf("coop exec process identity changed after owner-bound wake start"))
	}
	encodedOwner, ownerErr := encodeWakeOwnerEnv(finalOwner)
	if ownerErr != nil {
		return cleanupAfterError(fmt.Errorf("encode final wake owner: %w", ownerErr))
	}
	env = setEnvVar(unsetEnvVar(env, envWakeOwner), envWakeOwner, encodedOwner)
	if c.managedLaunchNonce == "" {
		env = codexDaemonSeatEnv(c.binaryPath, env, recorded)
	}
	var execErr error
	if c.managedLaunchNonce != "" {
		execErr = reexecManagedLaunchWrapper(c.root, c.agentHandle, c.managedLaunchNonce, c.binaryPath, argv, env, c.executionOptions)
	} else {
		execErr = coopExecProcess(c.binaryPath, argv, env)
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

	var current wakeLockInspection
	err = withWakeMutationScopeInDir(agentDir, wakeGuardExisting, func(scope *wakeMutationScope) error {
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
		_ = killWakeHelperProcessWithHandle(proc)
		if waitErr := waiter.waitForExit(wakeProcessExitTimeout); waitErr != nil {
			return errors.Join(err, waitErr)
		}
		return err
	}
	if agentDir == nil {
		_ = killWakeHelperProcessWithHandle(proc)
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
		_ = killWakeHelperProcessWithHandle(proc)
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
	_ = killWakeHelperProcessWithHandle(proc)
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
	return withWakeMutationScopeInDir(agentDir, wakeGuardExisting, func(scope *wakeMutationScope) error {
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

func cleanupTerminatedWakeLockInDir(agentDir *wakeAgentDir, expected wakeLockInspection) error {
	return withWakeMutationScopeInDir(agentDir, wakeGuardExisting, func(scope *wakeMutationScope) error {
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
	return withWakeMutationScopeInDir(agentDir, wakeGuardExisting, func(scope *wakeMutationScope) error {
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
