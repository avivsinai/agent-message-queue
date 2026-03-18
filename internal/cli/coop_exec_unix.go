//go:build darwin || linux

package cli

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/metadata"
)

func runCoopExec(args []string) error {
	// Split at "--" before flag parsing so agent flags aren't consumed.
	amqArgs, agentArgs := splitDashDash(args)

	fs := flag.NewFlagSet("coop exec", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "Root directory (override auto-detection)")
	sessionFlag := fs.String("session", "", "Session name (shorthand for --root .agent-mail/<name>)")
	meFlag := fs.String("me", "", "Agent handle (override auto-derivation from command name)")
	noInitFlag := fs.Bool("no-init", false, "Don't auto-initialize if .amqrc is missing")
	noWakeFlag := fs.Bool("no-wake", false, "Don't start amq wake in background")
	yesFlag := fs.Bool("y", false, "Skip confirmation prompts")
	topicFlag := fs.String("topic", "", "Session topic (written to session.json)")
	claimFlag := fs.String("claim", "", "Comma-separated session claims (written to session.json)")
	channelFlag := fs.String("channel", "", "Comma-separated channel memberships (written to agent.json)")

	usage := usageWithFlags(fs, "amq coop exec [options] <command> [-- <command-flags>]",
		"Set up co-op mode and exec into the agent (replaces this process).",
		"",
		"Sets AM_ROOT (always a session subdirectory), AM_ME, AM_PROJECT,",
		"AM_SESSION, and AM_BASE_ROOT, starts amq wake in background, then",
		"replaces itself with the given command via exec.",
		"",
		"Writes session.json (topic, branch, claims) and agent.json (channels)",
		"to the session root for federation discovery.",
		"",
		"If neither --session nor --root is given, defaults to --session collab.",
		"The agent handle is derived from the command basename unless --me is set.",
		"",
		"Examples:",
		"  amq coop exec claude                              # Exec into Claude Code (session=collab)",
		"  amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Codex with flags",
		"  amq coop exec --session feature-x claude          # Isolated session",
		"  amq coop exec --topic 'Auth rewrite' claude       # Session with topic",
		"  amq coop exec --channel ops,alerts codex          # Agent with channel memberships",
		"  amq coop exec --root .agent-mail/auth claude      # Explicit root (no session default)",
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
	var amqrcDir string // directory containing .amqrc (for resolving relative paths)
	var loadedAmqrc *amqrcResult
	if root == "" {
		existing, existingErr := findAndLoadAmqrc()
		switch existingErr {
		case nil:
			loadedAmqrc = &existing
			amqrcDir = existing.Dir
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

			if err := runCoopInitInternal(nil, false); err != nil {
				return fmt.Errorf("init failed: %w", err)
			}

			// Reload root after init.
			existing, existingErr := findAndLoadAmqrc()
			if existingErr != nil {
				return fmt.Errorf("failed to load .amqrc after init: %w", existingErr)
			}
			loadedAmqrc = &existing
			amqrcDir = existing.Dir
			root = existing.Config.Root
			if root != "" && !filepath.IsAbs(root) {
				root = filepath.Join(existing.Dir, root)
			}
		}
	}

	// Compute base root (before session suffix) for AM_BASE_ROOT.
	baseRoot := root

	// Default to --session collab when neither --session nor --root was specified.
	// This runs after auto-init so .amqrc exists and resolveBaseRoot() works.
	sessionName := *sessionFlag
	if *sessionFlag == "" && *rootFlag == "" {
		sessionName = defaultSessionName
		baseRoot = root // root is the literal .amqrc root (e.g., .agent-mail)
		root = filepath.Join(baseRoot, defaultSessionName)
		// Ensure session root + agent dirs exist.
		if err := fsq.EnsureRootDirs(root); err != nil {
			return fmt.Errorf("failed to create session root %q: %w", root, err)
		}
		if err := fsq.EnsureAgentDirs(root, agentHandle); err != nil {
			return fmt.Errorf("failed to create mailbox for %s at %q: %w", agentHandle, root, err)
		}
	} else if *sessionFlag != "" {
		// --session was specified; base root is the parent of root.
		baseRoot = filepath.Dir(root)
	} else {
		// --root was explicitly specified; derive session name from last path component.
		sessionName = filepath.Base(root)
		baseRoot = filepath.Dir(root)
	}

	// Ensure agent mailbox exists.
	if err := fsq.EnsureAgentDirs(root, agentHandle); err != nil {
		return fmt.Errorf("failed to ensure mailbox for %s: %w", agentHandle, err)
	}

	// Determine project name.
	projectName := ""
	if loadedAmqrc != nil && loadedAmqrc.Config.Project != "" {
		projectName = loadedAmqrc.Config.Project
	} else if amqrcDir != "" {
		projectName = filepath.Base(amqrcDir)
	} else {
		cwd, cwdErr := os.Getwd()
		if cwdErr == nil {
			projectName = filepath.Base(cwd)
		}
	}

	// Ensure .amqrc has a project_id; generate one if missing.
	ensureProjectID(loadedAmqrc, amqrcDir)

	// Write session.json metadata.
	writeSessionMetadata(root, sessionName, *topicFlag, *claimFlag)

	// Write agent.json metadata.
	writeAgentMetadata(root, agentHandle, *channelFlag)

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

		wakeCmd := exec.Command(amqBin, "--no-update-check", "wake", "--me", agentHandle, "--root", root)
		// Set AM_ROOT in wake's env so guardRootOverride doesn't conflict
		// with a stale AM_ROOT inherited from the parent process.
		wakeCmd.Env = setEnvVar(os.Environ(), envRoot, root)
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

	// Build environment with AM_ROOT, AM_ME, and federation env vars.
	// AM_ROOT always points to the session queue root (base/session), never the base.
	env := setEnvVar(os.Environ(), envRoot, root)
	env = setEnvVar(env, envMe, agentHandle)
	if projectName != "" {
		env = setEnvVar(env, envProject, projectName)
	}
	if sessionName != "" {
		env = setEnvVar(env, envSession, sessionName)
	}
	if baseRoot != "" {
		env = setEnvVar(env, envBaseRoot, baseRoot)
	}

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

// detectGitBranch returns the current git branch name, or "" if not in a git repo.
func detectGitBranch() string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// writeSessionMetadata writes session.json to the session root.
// Best-effort: failures are logged as warnings but do not abort exec.
func writeSessionMetadata(sessionRoot, session, topic, claimCSV string) {
	var claims []string
	if claimCSV != "" {
		claims = splitList(claimCSV)
	}

	branch := detectGitBranch()

	sm := metadata.SessionMeta{
		Session: session,
		Topic:   topic,
		Branch:  branch,
		Claims:  claims,
		Updated: time.Now().UTC(),
	}

	path := fsq.SessionJSON(sessionRoot)
	if err := metadata.WriteSessionMeta(path, sm); err != nil {
		_ = writeStderr("warning: failed to write session.json: %v\n", err)
	}
}

// writeAgentMetadata writes agent.json to the agent's directory.
// Best-effort: failures are logged as warnings but do not abort exec.
func writeAgentMetadata(root, agent, channelCSV string) {
	var channels []string
	if channelCSV != "" {
		channels = splitList(channelCSV)
	}

	am := metadata.AgentMeta{
		Agent:    agent,
		LastSeen: time.Now().UTC(),
		Channels: channels,
	}

	path := fsq.AgentJSON(root, agent)
	if err := metadata.WriteAgentMeta(path, am); err != nil {
		_ = writeStderr("warning: failed to write agent.json: %v\n", err)
	}
}

// ensureProjectID checks if .amqrc has a project_id field and generates one if missing.
// Best-effort: failures are logged as warnings.
func ensureProjectID(loaded *amqrcResult, amqrcDir string) {
	if loaded == nil {
		return
	}
	if loaded.Config.ProjectID != "" {
		return // Already has a project_id.
	}

	// Generate a UUID v4.
	id, err := generateUUID()
	if err != nil {
		_ = writeStderr("warning: failed to generate project_id: %v\n", err)
		return
	}

	// Update the in-memory config and write back.
	loaded.Config.ProjectID = id
	rcPath := filepath.Join(amqrcDir, ".amqrc")
	data, err := json.MarshalIndent(loaded.Config, "", "  ")
	if err != nil {
		_ = writeStderr("warning: failed to marshal .amqrc: %v\n", err)
		return
	}
	if err := os.WriteFile(rcPath, append(data, '\n'), 0o644); err != nil {
		_ = writeStderr("warning: failed to write .amqrc with project_id: %v\n", err)
	}
}

// generateUUID generates a UUID v4 string using crypto/rand.
func generateUUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	// Set version (4) and variant (RFC 4122).
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
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
