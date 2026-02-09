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

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runCoopExec(args []string) error {
	// Split at "--" before flag parsing so agent flags aren't consumed.
	amqArgs, agentArgs := splitDashDash(args)

	fs := flag.NewFlagSet("coop exec", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "Root directory (override auto-detection)")
	meFlag := fs.String("me", "", "Agent handle (override auto-derivation from command name)")
	noInitFlag := fs.Bool("no-init", false, "Don't auto-initialize if .amqrc is missing")
	noWakeFlag := fs.Bool("no-wake", false, "Don't start amq wake in background")
	yesFlag := fs.Bool("y", false, "Skip confirmation prompts")

	usage := usageWithFlags(fs, "amq coop exec [options] <command> [-- <command-flags>]",
		"Set up co-op mode and exec into the agent (replaces this process).",
		"",
		"Sets AM_ROOT and AM_ME, starts amq wake in background, then",
		"replaces itself with the given command via exec.",
		"",
		"The agent handle is derived from the command basename unless --me is set.",
		"",
		"Examples:",
		"  amq coop exec claude                              # Exec into Claude Code",
		"  amq coop exec codex -- --dangerously-skip-permissions  # Codex with flags",
		"  amq coop exec --root .agent-mail/auth claude      # Isolated session",
		"  amq coop exec --me myagent bash                   # Debug shell with AMQ env",
	)

	if handled, err := parseFlags(fs, amqArgs, usage); err != nil {
		return err
	} else if handled {
		return nil
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
	agentHandle, err := normalizeHandle(agentHandle)
	if err != nil {
		return fmt.Errorf("cannot derive agent handle from %q: %w (use --me to override)", cmdName, err)
	}

	// Resolve root: --root flag > .amqrc > default.
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

	// Auto-init if needed.
	if root == "" || !dirExists(root) {
		if *noInitFlag {
			if root == "" {
				return fmt.Errorf("no .amqrc found and no --root specified; run 'amq coop init' first or remove --no-init")
			}
			return fmt.Errorf("root %q does not exist; run 'amq coop init' first or remove --no-init", root)
		}

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
		if *rootFlag != "" {
			initArgs = append(initArgs, "--root", *rootFlag)
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

		wakeCmd := exec.Command(amqBin, "wake", "--me", agentHandle, "--root", root)
		wakeCmd.Stdin = os.Stdin
		wakeCmd.Stdout = os.Stdout
		wakeCmd.Stderr = os.Stderr

		if err := wakeCmd.Start(); err != nil {
			_ = writeStderr("warning: failed to start amq wake: %v\n", err)
		} else {
			wakeProc = wakeCmd.Process
			_ = writeStderr("Started amq wake (pid %d)\n", wakeProc.Pid)
		}
	}

	// Build environment with AM_ROOT and AM_ME.
	env := setEnvVar(os.Environ(), envRoot, root)
	env = setEnvVar(env, envMe, agentHandle)

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

// setEnvVar sets or replaces an environment variable in a slice.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, v := range env {
		if strings.HasPrefix(v, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
