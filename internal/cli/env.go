package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// amqrc represents the .amqrc configuration file format.
// Root is the literal queue root directory (e.g., ".agent-mail").
// Agent identity ('me') should be set per-terminal via AM_ME env var or --me flag.
type amqrc struct {
	Root    string            `json:"root"`
	Project string            `json:"project,omitempty"` // explicit project name (defaults to directory basename)
	Peers   map[string]string `json:"peers,omitempty"`   // peer name → peer's base root path
}

// amqrcResult holds both the parsed config and the directory where it was found.
type amqrcResult struct {
	Config amqrc
	Dir    string // Directory containing .amqrc (for resolving relative paths)
}

// envOutput is the JSON output format for amq env --json.
type envOutput struct {
	Root  string `json:"root,omitempty"`
	Me    string `json:"me,omitempty"`
	Shell string `json:"shell,omitempty"`
	Wake  bool   `json:"wake,omitempty"`
}

// errAmqrcNotFound is returned when .amqrc is not found (non-fatal).
var errAmqrcNotFound = errors.New(".amqrc not found")

func runEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	meFlag := fs.String("me", "", "Agent handle (overrides AM_ME)")
	rootFlag := fs.String("root", "", "Root directory (overrides .amqrc and AM_ROOT)")
	sessionFlag := fs.String("session", "", "Session name (shorthand for --root .agent-mail/<name>)")
	shellFlag := fs.String("shell", "sh", "Shell format: sh, bash, zsh, fish")
	wakeFlag := fs.Bool("wake", false, "Include amq wake & in output")
	jsonFlag := fs.Bool("json", false, "Output as JSON (for scripts)")

	usage := usageWithFlags(fs, "amq env [options]",
		"Outputs shell commands to set AM_ROOT and AM_ME environment variables.",
		"",
		"Configuration precedence (highest to lowest):",
		"  Root: flags > env (AM_ROOT) > .amqrc > auto-detect (.agent-mail/)",
		"  Me:   flags > env (AM_ME)",
		"",
		"Note: .amqrc only configures 'root'. Agent identity ('me') is set",
		"per-terminal via --me or AM_ME, since different terminals may use",
		"different agents on the same project.",
		"",
		"Examples:",
		"  eval \"$(amq env --me claude)\"                # Set up for Claude",
		"  eval \"$(amq env --me codex --wake)\"          # Set up for Codex with wake",
		"  eval \"$(amq env --session feature-x --me claude)\"  # Isolated session",
		"  amq env --json                                # Machine-readable output",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	// Resolve --session into --root (mutually exclusive).
	if *sessionFlag != "" {
		if *rootFlag != "" {
			return UsageError("--session and --root are mutually exclusive")
		}
		if err := validateSessionName(*sessionFlag); err != nil {
			return err
		}
		// Resolve base from .amqrc or default
		base := resolveBaseRoot()
		*rootFlag = filepath.Join(base, *sessionFlag)
	}

	// Resolve configuration with precedence
	root, me, err := resolveEnvConfig(*rootFlag, *meFlag)
	if err != nil {
		return err
	}

	// Validate shell
	shell := strings.ToLower(strings.TrimSpace(*shellFlag))
	if !isValidShell(shell) {
		return UsageError("invalid shell %q (supported: sh, bash, zsh, fish)", shell)
	}

	// JSON output mode
	if *jsonFlag {
		out := envOutput{
			Root:  root,
			Me:    me,
			Shell: shell,
			Wake:  *wakeFlag,
		}
		return writeJSON(os.Stdout, out)
	}

	// Generate shell commands
	return writeShellEnv(root, me, shell, *wakeFlag)
}

// resolveEnvConfig resolves root and me with proper precedence.
// Precedence:
//   - Root: flags > env > .amqrc > auto-detect
//   - Me:   flags > env (NOT from .amqrc)
func resolveEnvConfig(rootFlag, meFlag string) (string, string, error) {
	var root, me string

	// Collect values from all sources, then apply precedence

	// 1. Try .amqrc file (for root only, lowest precedence)
	var rcErr error
	var rcRoot string
	rcResult, err := findAndLoadAmqrc()
	if err != nil {
		if !errors.Is(err, errAmqrcNotFound) {
			// Save the error - we'll report it only if no higher-precedence source provides values
			rcErr = err
		}
		// Not found is fine, continue with other sources
	} else {
		rcRoot = rcResult.Config.Root

		// Note: 'me' is intentionally not read from .amqrc
		// Different terminals on the same project may need different agent identities

		// Resolve relative path against .amqrc directory
		if rcRoot != "" && !filepath.IsAbs(rcRoot) {
			rcRoot = filepath.Join(rcResult.Dir, rcRoot)
		}
	}

	// 2. Auto-detect .agent-mail/ directory
	autoRoot := detectAgentMailDir()

	// 3. Environment variables
	envRootVal := strings.TrimSpace(os.Getenv(envRoot))
	envMeVal := strings.TrimSpace(os.Getenv(envMe))

	// 4. Command-line flags (already have rootFlag, meFlag)

	// Now apply precedence for root: flags > env > .amqrc > auto-detect
	if rootFlag != "" {
		root = rootFlag
	} else if envRootVal != "" {
		root = envRootVal
	} else if rcRoot != "" {
		root = rcRoot
	} else if autoRoot != "" {
		root = autoRoot
	}

	// Apply precedence for me: flags > env (NOT from .amqrc)
	if meFlag != "" {
		me = meFlag
	} else if envMeVal != "" {
		me = envMeVal
	}

	// If we would have needed .amqrc values but it was invalid, report the error
	// Only error if .amqrc was invalid AND no higher-precedence source provided root
	// Note: Only flags and env vars are higher precedence than .amqrc; auto-detect is lower
	if rcErr != nil {
		// Check if a higher-precedence source (flags or env) provided root
		hasHigherPrecedenceRoot := rootFlag != "" || envRootVal != ""
		if !hasHigherPrecedenceRoot {
			return "", "", rcErr
		}
		// Otherwise, warn but continue (higher-precedence source provided values)
		_ = writeStderr("warning: %v (using override from flags/env)\n", rcErr)
	}

	// Validate we have at least root
	if root == "" {
		return "", "", fmt.Errorf("cannot determine root: no .amqrc found, no .agent-mail/ directory, and AM_ROOT not set")
	}

	// Normalize and validate me if provided
	if me != "" {
		normalized, err := normalizeHandle(me)
		if err != nil {
			return "", "", fmt.Errorf("invalid agent handle: %w", err)
		}
		me = normalized
	}

	return root, me, nil
}

// resolveBaseRoot returns the base root directory (without session suffix).
// Tries .amqrc first, then falls back to defaultCoopRoot.
func resolveBaseRoot() string {
	result, err := findAndLoadAmqrc()
	if err == nil && result.Config.Root != "" {
		base := result.Config.Root
		if !filepath.IsAbs(base) {
			base = filepath.Join(result.Dir, base)
		}
		return base
	}
	return defaultCoopRoot
}

// findAndLoadAmqrc searches for .amqrc in current and parent directories.
// Returns errAmqrcNotFound if no .amqrc exists (non-fatal).
// Returns other errors for invalid/unreadable .amqrc (fatal).
func findAndLoadAmqrc() (amqrcResult, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return amqrcResult{}, err
	}

	dir := cwd
	for {
		rcPath := filepath.Join(dir, ".amqrc")
		data, err := os.ReadFile(rcPath)
		if err != nil {
			if os.IsNotExist(err) {
				// Keep searching in parent directories
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
				continue
			}
			// Other read errors (permissions, etc.) are fatal
			return amqrcResult{}, fmt.Errorf("cannot read .amqrc at %s: %w", rcPath, err)
		}

		// File exists, try to parse it
		var rc amqrc
		if err := json.Unmarshal(data, &rc); err != nil {
			return amqrcResult{}, fmt.Errorf("invalid .amqrc at %s: %w", rcPath, err)
		}
		return amqrcResult{Config: rc, Dir: dir}, nil
	}

	return amqrcResult{}, errAmqrcNotFound
}

// detectAgentMailDir searches for .agent-mail/ in current and parent directories.
func detectAgentMailDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	dir := cwd
	for {
		candidate := filepath.Join(dir, ".agent-mail")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			// Return relative path if in cwd, absolute otherwise
			if dir == cwd {
				return ".agent-mail"
			}
			return candidate
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return ""
}

func isValidShell(shell string) bool {
	switch shell {
	case "sh", "bash", "zsh", "fish":
		return true
	default:
		return false
	}
}

func writeShellEnv(root, me, shell string, wake bool) error {
	switch shell {
	case "fish":
		return writeFishEnv(root, me, wake)
	default:
		return writePosixEnv(root, me, wake)
	}
}

func writePosixEnv(root, me string, wake bool) error {
	// Use proper shell quoting
	if root != "" {
		if err := writeStdout("export AM_ROOT=%s\n", shellQuotePosix(root)); err != nil {
			return err
		}
	}
	if me != "" {
		if err := writeStdout("export AM_ME=%s\n", shellQuotePosix(me)); err != nil {
			return err
		}
	}
	if wake {
		if err := writeStdoutLine("amq wake &"); err != nil {
			return err
		}
	}
	return nil
}

func writeFishEnv(root, me string, wake bool) error {
	if root != "" {
		if err := writeStdout("set -gx AM_ROOT %s\n", shellQuoteFish(root)); err != nil {
			return err
		}
	}
	if me != "" {
		if err := writeStdout("set -gx AM_ME %s\n", shellQuoteFish(me)); err != nil {
			return err
		}
	}
	if wake {
		if err := writeStdoutLine("amq wake &"); err != nil {
			return err
		}
	}
	return nil
}

// shellQuotePosix quotes a string for safe use in POSIX shell commands.
// Uses single quotes with proper escaping.
func shellQuotePosix(s string) string {
	// If string contains no special characters, return as-is
	if isSimpleString(s) {
		return s
	}
	// Use single quotes, escaping any single quotes in the string
	// The pattern 'foo'\''bar' closes the quote, adds escaped quote, reopens
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// shellQuoteFish quotes a string for safe use in fish shell commands.
// Fish uses different escaping rules than POSIX shells.
func shellQuoteFish(s string) string {
	// If string contains no special characters, return as-is
	if isSimpleString(s) {
		return s
	}
	// In fish, single quotes work but single quotes inside need escaping with backslash
	// Unlike POSIX, fish allows \' inside single-quoted strings
	return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
}

func isSimpleString(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' || r == '/' {
			continue
		}
		return false
	}
	return true
}

// findAmqrcForRoot tries to locate the .amqrc for the given root.
// First tries the standard cwd-based search (findAndLoadAmqrc), then falls
// back to walking up from the root path. This allows cross-project commands
// to work even when invoked from outside the project tree.
func findAmqrcForRoot(root string) (amqrcResult, error) {
	result, err := findAndLoadAmqrc()
	if err == nil {
		return result, nil
	}
	if root == "" {
		return amqrcResult{}, err
	}
	// Fallback: walk up from root's directory to find .amqrc.
	absRoot, absErr := filepath.Abs(root)
	if absErr != nil {
		return amqrcResult{}, err // return original error
	}
	dir := absRoot
	for {
		rcPath := filepath.Join(dir, ".amqrc")
		data, readErr := os.ReadFile(rcPath)
		if readErr == nil {
			var rc amqrc
			if jsonErr := json.Unmarshal(data, &rc); jsonErr != nil {
				return amqrcResult{}, fmt.Errorf("invalid .amqrc at %s: %w", rcPath, jsonErr)
			}
			return amqrcResult{Config: rc, Dir: dir}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return amqrcResult{}, err // return original cwd-based error
}

// resolvePeer looks up a peer project name in the .amqrc peers map and returns
// the absolute base root path for that peer. Returns an error if .amqrc is not
// found, has no peers, or the peer name is not registered.
func resolvePeer(root, project string) (string, error) {
	result, err := findAmqrcForRoot(root)
	if err != nil {
		return "", fmt.Errorf("cannot resolve peer %q: %w", project, err)
	}
	if len(result.Config.Peers) == 0 {
		return "", fmt.Errorf("no peers configured in .amqrc (looking for %q)", project)
	}
	peerPath, ok := result.Config.Peers[project]
	if !ok {
		known := make([]string, 0, len(result.Config.Peers))
		for k := range result.Config.Peers {
			known = append(known, k)
		}
		return "", fmt.Errorf("peer %q not found in .amqrc (known: %v)", project, known)
	}
	if !filepath.IsAbs(peerPath) {
		peerPath = filepath.Join(result.Dir, peerPath)
	}
	abs, err := filepath.Abs(peerPath)
	if err != nil {
		return "", fmt.Errorf("resolve peer path for %q: %w", project, err)
	}
	return abs, nil
}

// resolveProject returns the project name for the current .amqrc.
// Uses the explicit "project" field if set, otherwise falls back to the
// basename of the directory containing .amqrc.
func resolveProject(root string) string {
	result, err := findAmqrcForRoot(root)
	if err != nil {
		return ""
	}
	if result.Config.Project != "" {
		return result.Config.Project
	}
	return filepath.Base(result.Dir)
}
