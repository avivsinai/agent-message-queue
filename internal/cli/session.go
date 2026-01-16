package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// sessionData represents the session file format.
type sessionData struct {
	Root string `json:"root"`
	Me   string `json:"me"`
	Wake int    `json:"wake,omitempty"` // PID of wake process if running
}

const sessionFileName = ".session"

func runSession(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printSessionUsage()
	}
	switch args[0] {
	case "start":
		return runSessionStart(args[1:])
	case "stop":
		return runSessionStop(args[1:])
	case "status":
		return runSessionStatus(args[1:])
	default:
		return fmt.Errorf("unknown session command: %s", args[0])
	}
}

func runSessionStart(args []string) error {
	fs := flag.NewFlagSet("session start", flag.ContinueOnError)
	meFlag := fs.String("me", "", "Agent handle (required)")
	rootFlag := fs.String("root", "", "Root directory (optional, auto-detected from .amqrc or .agent-mail/)")
	noWake := fs.Bool("no-wake", false, "Don't start amq wake")

	usage := usageWithFlags(fs, "amq session start --me <agent> [options]",
		"Starts a new AMQ session for the specified agent.",
		"",
		"This command:",
		"  1. Resolves root from --root, .amqrc, or .agent-mail/ directory",
		"  2. Writes a session file that amq commands will read",
		"  3. Starts amq wake in the background (unless --no-wake)",
		"",
		"After running this, all amq commands will use the session config.",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	// Require --me
	me := strings.TrimSpace(*meFlag)
	if me == "" {
		return UsageError("--me is required")
	}
	normalized, err := normalizeHandle(me)
	if err != nil {
		return fmt.Errorf("invalid agent handle: %w", err)
	}
	me = normalized

	// Resolve root
	root, err := resolveRootForSession(*rootFlag)
	if err != nil {
		return err
	}

	// Make root absolute for session file
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("cannot resolve absolute path for root: %w", err)
	}

	// Check if session already exists
	sessionPath := filepath.Join(absRoot, sessionFileName)
	if _, err := os.Stat(sessionPath); err == nil {
		existing, loadErr := loadSession(absRoot)
		if loadErr == nil {
			return fmt.Errorf("session already active (me=%s). Run 'amq session stop' first", existing.Me)
		}
	}

	// Start wake if requested
	var wakePID int
	if !*noWake {
		pid, err := startWakeProcess(absRoot, me)
		if err != nil {
			_ = writeStderr("warning: failed to start wake: %v\n", err)
		} else {
			wakePID = pid
		}
	}

	// Write session file
	session := sessionData{
		Root: absRoot,
		Me:   me,
		Wake: wakePID,
	}
	if err := writeSession(absRoot, session); err != nil {
		// Try to kill wake if we started it
		if wakePID > 0 {
			_ = syscall.Kill(wakePID, syscall.SIGTERM)
		}
		return err
	}

	if err := writeStdout("Session started: me=%s, root=%s\n", me, absRoot); err != nil {
		return err
	}
	if wakePID > 0 {
		if err := writeStdout("Wake process started (PID %d)\n", wakePID); err != nil {
			return err
		}
	}
	return nil
}

func runSessionStop(args []string) error {
	fs := flag.NewFlagSet("session stop", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "Root directory (optional, auto-detected)")

	usage := usageWithFlags(fs, "amq session stop [options]",
		"Stops the current AMQ session.",
		"",
		"This command:",
		"  1. Kills the wake process if running",
		"  2. Removes the session file",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	// Resolve root
	root, err := resolveRootForSession(*rootFlag)
	if err != nil {
		return err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("cannot resolve absolute path for root: %w", err)
	}

	// Load existing session
	session, err := loadSession(absRoot)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			return fmt.Errorf("no active session found")
		}
		return err
	}

	// Kill wake process if running
	if session.Wake > 0 {
		if err := syscall.Kill(session.Wake, syscall.SIGTERM); err != nil {
			if !errors.Is(err, syscall.ESRCH) { // ESRCH = no such process
				_ = writeStderr("warning: failed to kill wake process: %v\n", err)
			}
		} else {
			if err := writeStdout("Stopped wake process (PID %d)\n", session.Wake); err != nil {
				return err
			}
		}
	}

	// Remove session file
	sessionPath := filepath.Join(absRoot, sessionFileName)
	if err := os.Remove(sessionPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove session file: %w", err)
	}

	return writeStdoutLine("Session stopped.")
}

func runSessionStatus(args []string) error {
	fs := flag.NewFlagSet("session status", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "Root directory (optional, auto-detected)")
	jsonFlag := fs.Bool("json", false, "Output as JSON")

	usage := usageWithFlags(fs, "amq session status [options]",
		"Shows the current session status.",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	// Resolve root
	root, err := resolveRootForSession(*rootFlag)
	if err != nil {
		if *jsonFlag {
			return writeJSON(os.Stdout, map[string]any{"active": false, "error": err.Error()})
		}
		return fmt.Errorf("no session: %w", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("cannot resolve absolute path for root: %w", err)
	}

	// Load session
	session, err := loadSession(absRoot)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			if *jsonFlag {
				return writeJSON(os.Stdout, map[string]any{"active": false, "root": absRoot})
			}
			return writeStdout("No active session (root=%s)\n", absRoot)
		}
		return err
	}

	// Check if wake is still running
	wakeRunning := false
	if session.Wake > 0 {
		if err := syscall.Kill(session.Wake, 0); err == nil {
			wakeRunning = true
		}
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, map[string]any{
			"active":       true,
			"root":         session.Root,
			"me":           session.Me,
			"wake_pid":     session.Wake,
			"wake_running": wakeRunning,
		})
	}

	if err := writeStdout("Session active:\n"); err != nil {
		return err
	}
	if err := writeStdout("  root: %s\n", session.Root); err != nil {
		return err
	}
	if err := writeStdout("  me:   %s\n", session.Me); err != nil {
		return err
	}
	if session.Wake > 0 {
		status := "running"
		if !wakeRunning {
			status = "stopped"
		}
		if err := writeStdout("  wake: PID %d (%s)\n", session.Wake, status); err != nil {
			return err
		}
	}
	return nil
}

func printSessionUsage() error {
	if err := writeStdoutLine("amq session <command> [options]"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Commands:"); err != nil {
		return err
	}
	if err := writeStdoutLine("  start   Start a new session"); err != nil {
		return err
	}
	if err := writeStdoutLine("  stop    Stop the current session"); err != nil {
		return err
	}
	if err := writeStdoutLine("  status  Show session status"); err != nil {
		return err
	}
	return nil
}

// resolveRootForSession resolves the root directory for session commands.
// Checks: flag > env > .amqrc > auto-detect
func resolveRootForSession(rootFlag string) (string, error) {
	if rootFlag != "" {
		return rootFlag, nil
	}

	if envRoot := strings.TrimSpace(os.Getenv(envRoot)); envRoot != "" {
		return envRoot, nil
	}

	// Try .amqrc (only for root, not me)
	if rcResult, err := findAndLoadAmqrc(); err == nil {
		root := rcResult.Config.Root
		if root != "" {
			if !filepath.IsAbs(root) {
				root = filepath.Join(rcResult.Dir, root)
			}
			return root, nil
		}
	}

	// Try auto-detect
	if autoRoot := detectAgentMailDir(); autoRoot != "" {
		return autoRoot, nil
	}

	return "", fmt.Errorf("cannot determine root: no .amqrc found, no .agent-mail/ directory, and AM_ROOT not set")
}

var errSessionNotFound = errors.New("session not found")

// loadSession loads the session file from the given root.
func loadSession(root string) (sessionData, error) {
	sessionPath := filepath.Join(root, sessionFileName)
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionData{}, errSessionNotFound
		}
		return sessionData{}, fmt.Errorf("cannot read session file: %w", err)
	}

	var session sessionData
	if err := json.Unmarshal(data, &session); err != nil {
		return sessionData{}, fmt.Errorf("invalid session file: %w", err)
	}
	return session, nil
}

// writeSession writes the session file to the given root.
func writeSession(root string, session sessionData) error {
	sessionPath := filepath.Join(root, sessionFileName)
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot marshal session: %w", err)
	}
	if err := os.WriteFile(sessionPath, data, 0o600); err != nil {
		return fmt.Errorf("cannot write session file: %w", err)
	}
	return nil
}

// findSessionRoot looks for an active session by checking current dir and parents.
func findSessionRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	dir := cwd
	for {
		sessionPath := filepath.Join(dir, sessionFileName)
		if _, err := os.Stat(sessionPath); err == nil {
			return dir, nil
		}

		// Also check inside .agent-mail/
		agentMailSession := filepath.Join(dir, ".agent-mail", sessionFileName)
		if _, err := os.Stat(agentMailSession); err == nil {
			return filepath.Join(dir, ".agent-mail"), nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", errSessionNotFound
}

// startWakeProcess starts amq wake in the background and returns its PID.
func startWakeProcess(root, me string) (int, error) {
	// Find amq binary
	amqPath, err := exec.LookPath("amq")
	if err != nil {
		return 0, fmt.Errorf("amq not found in PATH: %w", err)
	}

	cmd := exec.Command(amqPath, "wake", "--root", root, "--me", me)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // New process group so Ctrl-C doesn't kill it
	}

	if err := cmd.Start(); err != nil {
		return 0, err
	}

	return cmd.Process.Pid, nil
}

// sessionConfigResult holds resolved config from session.
type sessionConfigResult struct {
	Root string
	Me   string
}

// resolveFromSession tries to resolve root and me from session file.
// Returns empty strings if no session found.
func resolveFromSession() (sessionConfigResult, error) {
	// First try to find session in current directory's root
	root, err := findSessionRoot()
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			return sessionConfigResult{}, nil
		}
		return sessionConfigResult{}, err
	}

	session, err := loadSession(root)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			return sessionConfigResult{}, nil
		}
		return sessionConfigResult{}, err
	}

	return sessionConfigResult{
		Root: session.Root,
		Me:   session.Me,
	}, nil
}

// GetSessionPID returns the wake PID from the session file, or 0 if not found.
func GetSessionPID(root string) int {
	session, err := loadSession(root)
	if err != nil {
		return 0
	}
	return session.Wake
}

// IsSessionActive checks if there's an active session.
func IsSessionActive(root string) bool {
	_, err := loadSession(root)
	return err == nil
}

// SessionFilePath returns the path to the session file for the given root.
func SessionFilePath(root string) string {
	return filepath.Join(root, sessionFileName)
}
