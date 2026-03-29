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
	Root    string            `json:"root,omitempty"`
	Me      string            `json:"me,omitempty"`
	Shell   string            `json:"shell,omitempty"`
	Wake    bool              `json:"wake,omitempty"`
	Project string            `json:"project,omitempty"`
	Peers   map[string]string `json:"peers,omitempty"`
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
		"  Root: flags > env (AM_ROOT) > .amqrc > AMQ_GLOBAL_ROOT > ~/.amqrc > auto-detect",
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
		// Include project identity and peer config from .amqrc
		// so agents can discover cross-project routing without
		// reading .amqrc directly.
		if rcResult, rcErr := findAmqrcForRoot(root); rcErr == nil {
			out.Project = rcResult.Config.Project
			if out.Project == "" {
				// Only infer project from directory basename for
				// project-local .amqrc, not global ~/.amqrc (which
				// is a queue locator, not a project identity).
				home, _ := os.UserHomeDir()
				if home == "" || rcResult.Dir != home {
					out.Project = filepath.Base(rcResult.Dir)
				}
			}
			if len(rcResult.Config.Peers) > 0 {
				out.Peers = rcResult.Config.Peers
			}
		}
		return writeJSON(os.Stdout, out)
	}

	// Generate shell commands
	return writeShellEnv(root, me, shell, *wakeFlag)
}

// rootSource describes which configuration source provided the resolved root.
type rootSource string

const (
	rootSourceFlag       rootSource = "flag"
	rootSourceEnv        rootSource = "env"
	rootSourceProjectRC  rootSource = "project_amqrc"
	rootSourceGlobalEnv  rootSource = "global_env"
	rootSourceGlobalRC   rootSource = "global_amqrc"
	rootSourceAutoDetect rootSource = "auto_detect"
)

// resolveEnvConfig resolves root and me with proper precedence.
// Precedence:
//   - Root: flags > env > .amqrc > AMQ_GLOBAL_ROOT > ~/.amqrc > auto-detect
//   - Me:   flags > env (NOT from .amqrc)
func resolveEnvConfig(rootFlag, meFlag string) (string, string, error) {
	root, _, me, err := resolveEnvConfigWithSource(rootFlag, meFlag)
	return root, me, err
}

// resolveEnvConfigWithSource resolves root and me, returning the winning source for root.
func resolveEnvConfigWithSource(rootFlag, meFlag string) (string, rootSource, string, error) {
	var root, me string
	var source rootSource

	// Collect values from all sources, then apply precedence

	// 1. Try project .amqrc file (for root only)
	var rcErr error
	var rcRoot string
	rcResult, err := findAndLoadAmqrc()
	if err != nil {
		if !errors.Is(err, errAmqrcNotFound) {
			rcErr = err
		}
	} else {
		rcRoot = rcResult.Config.Root
		if rcRoot != "" && !filepath.IsAbs(rcRoot) {
			rcRoot = filepath.Join(rcResult.Dir, rcRoot)
		}
	}

	// 2. Try global root fallback (AMQ_GLOBAL_ROOT env var)
	globalEnvRoot := strings.TrimSpace(os.Getenv(envGlobalRoot))

	// 3. Try global ~/.amqrc
	var globalRCRoot string
	globalResult, err := loadGlobalAmqrc()
	if err == nil && globalResult.Config.Root != "" {
		globalRCRoot = globalResult.Config.Root
		if !filepath.IsAbs(globalRCRoot) {
			globalRCRoot = filepath.Join(globalResult.Dir, globalRCRoot)
		}
	}

	// 4. Auto-detect .agent-mail/ directory
	autoRoot := detectAgentMailDir()

	// 5. Environment variables
	envRootVal := strings.TrimSpace(os.Getenv(envRoot))
	envMeVal := strings.TrimSpace(os.Getenv(envMe))

	// 6. Command-line flags (already have rootFlag, meFlag)

	// Apply precedence: flags > env > project .amqrc > AMQ_GLOBAL_ROOT > ~/.amqrc > auto-detect
	switch {
	case rootFlag != "":
		root, source = rootFlag, rootSourceFlag
	case envRootVal != "":
		root, source = envRootVal, rootSourceEnv
	case rcRoot != "":
		root, source = rcRoot, rootSourceProjectRC
	case globalEnvRoot != "":
		root, source = globalEnvRoot, rootSourceGlobalEnv
	case globalRCRoot != "":
		root, source = globalRCRoot, rootSourceGlobalRC
	case autoRoot != "":
		root, source = autoRoot, rootSourceAutoDetect
	}

	// Apply precedence for me: flags > env (NOT from .amqrc)
	if meFlag != "" {
		me = meFlag
	} else if envMeVal != "" {
		me = envMeVal
	}

	// Report .amqrc errors only if no higher-precedence source provided root
	if rcErr != nil {
		hasHigherPrecedenceRoot := rootFlag != "" || envRootVal != ""
		if !hasHigherPrecedenceRoot {
			return "", "", "", rcErr
		}
		_ = writeStderr("warning: %v (using override from flags/env)\n", rcErr)
	}

	if root == "" {
		return "", "", "", fmt.Errorf("cannot determine root: no .amqrc found, no .agent-mail/ directory, AM_ROOT not set, and no global config (~/.amqrc or AMQ_GLOBAL_ROOT)")
	}

	if me != "" {
		normalized, err := normalizeHandle(me)
		if err != nil {
			return "", "", "", fmt.Errorf("invalid agent handle: %w", err)
		}
		me = normalized
	}

	return root, source, me, nil
}

// loadGlobalAmqrc loads ~/.amqrc if it exists.
func loadGlobalAmqrc() (amqrcResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return amqrcResult{}, errAmqrcNotFound
	}
	path := filepath.Join(home, ".amqrc")
	data, err := os.ReadFile(path)
	if err != nil {
		return amqrcResult{}, errAmqrcNotFound
	}
	var cfg amqrc
	if err := json.Unmarshal(data, &cfg); err != nil {
		return amqrcResult{}, fmt.Errorf("invalid ~/.amqrc: %w", err)
	}
	return amqrcResult{Config: cfg, Dir: home}, nil
}

// resolveBaseRoot returns the base root directory (without session suffix).
// Tries project .amqrc first, then AMQ_GLOBAL_ROOT, then ~/.amqrc, then defaultCoopRoot.
func resolveBaseRoot() string {
	// 1. Project .amqrc
	result, err := findAndLoadAmqrc()
	if err == nil && result.Config.Root != "" {
		base := result.Config.Root
		if !filepath.IsAbs(base) {
			base = filepath.Join(result.Dir, base)
		}
		return base
	}

	// 2. AMQ_GLOBAL_ROOT env var
	if globalEnv := strings.TrimSpace(os.Getenv(envGlobalRoot)); globalEnv != "" {
		return globalEnv
	}

	// 3. Global ~/.amqrc
	globalResult, err := loadGlobalAmqrc()
	if err == nil && globalResult.Config.Root != "" {
		base := globalResult.Config.Root
		if !filepath.IsAbs(base) {
			base = filepath.Join(globalResult.Dir, base)
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

// findAmqrcForRoot locates the .amqrc for the given root.
// When root is provided (non-empty), root-based lookup takes priority over
// cwd-based search. This ensures --root / AM_ROOT fully determines which
// project config is used, even when cwd is inside a different project.
func findAmqrcForRoot(root string) (amqrcResult, error) {
	// When root is provided, search from root first (authoritative).
	if root != "" {
		absRoot, absErr := filepath.Abs(root)
		if absErr == nil {
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
				if !os.IsNotExist(readErr) {
					// Permission or I/O error — report it, don't mask it.
					return amqrcResult{}, fmt.Errorf("cannot read .amqrc at %s: %w", rcPath, readErr)
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
		}
	}
	// Fall back to cwd-based search (standard behavior when root is empty
	// or root-based search found nothing).
	return findAndLoadAmqrc()
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
