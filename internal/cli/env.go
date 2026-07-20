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
	Path   string // Canonical path of the config file (shared provenance)
}

// envOutput is the JSON output format for amq env --json.
type envOutput struct {
	SchemaVersion int               `json:"schema_version"`
	AMQVersion    string            `json:"amq_version"`
	Root          string            `json:"root"`
	RootID        string            `json:"root_id,omitempty"`
	BaseRoot      string            `json:"base_root"`
	BaseRootID    string            `json:"base_root_id,omitempty"`
	SessionName   string            `json:"session_name"`
	InSession     bool              `json:"in_session"`
	Me            string            `json:"me"`
	Project       string            `json:"project"`
	RootSource    string            `json:"root_source"`
	Peers         map[string]string `json:"peers"`
	Shell         string            `json:"shell,omitempty"`
	Wake          bool              `json:"wake,omitempty"`
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
	exportFlag := fs.Bool("export", false, "Also print a note confirming the resolved terminal pin")
	sessionNameFlag := fs.Bool("session-name", false, "Print current session name (for statusline integration)")

	usage := usageWithFlags(fs, "amq env [options]",
		"Outputs shell commands that replace the complete AMQ root/session context.",
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
		"  eval \"$(amq env --me claude)\"                # Set up Claude and clear stale session context",
		"  eval \"$(amq env --session feature-x --me claude --export)\"  # Pin this terminal to one session",
		"  eval \"$(amq env --me codex --wake)\"          # Set up for Codex with wake",
		"  eval \"$(amq env --session feature-x --me claude)\"  # Isolated session",
		"  amq env --json                                # Machine-readable output",
		"  amq env --session-name                         # Print session name (for statusline)",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	// --session-name and --json are mutually exclusive output modes.
	if *sessionNameFlag && *jsonFlag {
		return UsageError("--session-name and --json are mutually exclusive")
	}
	if *exportFlag && *jsonFlag {
		return UsageError("--export and --json are mutually exclusive")
	}
	if *exportFlag && *sessionNameFlag {
		return UsageError("--export and --session-name are mutually exclusive")
	}
	contextExplicit := flagWasVisited(fs, "root") || flagWasVisited(fs, "session")

	// Resolve --session into --root (mutually exclusive).
	sessionBaseRoot := ""
	sessionNameOverride := ""
	if *sessionFlag != "" {
		if *rootFlag != "" {
			return UsageError("--session and --root are mutually exclusive")
		}
		if err := validateSessionName(*sessionFlag); err != nil {
			return err
		}
		pin, err := loadSessionPin()
		if err != nil {
			return err
		}
		base := ""
		if pin.Present {
			if pin.IdentityPin {
				ambient := strings.TrimSpace(os.Getenv(envRoot))
				if ambient == "" {
					ambient = pin.ExpectedRoot
				}
				if err := verifyRootUnderBase(pin.BaseRoot, pin.BaseRootID, pin.Session, ambient, pin.RootID); err != nil {
					return err
				}
				if relation := verifyTreeIdentityToken(pin.BaseRoot, pin.BaseRootID); relation != TreeRelationSame {
					return ContextMismatchError("refusing env session route: pinned base root identity is %s for %s", relation, pin.BaseRoot)
				}
				entry := filepath.Join(pin.BaseRoot, *sessionFlag)
				info, statErr := os.Lstat(entry)
				if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return ContextMismatchError("refusing env session route: %q is not a direct directory under pinned base", *sessionFlag)
				}
			}
			base = pin.BaseRoot
		} else {
			resolved, _, _, err := resolveEnvConfigWithSource("", *meFlag)
			if err != nil {
				return err
			}
			base = baseRootOf(absPath(resolveRoot(resolved)))
		}
		sessionBaseRoot = absPath(resolveRoot(base))
		sessionNameOverride = *sessionFlag
		*rootFlag = filepath.Join(sessionBaseRoot, *sessionFlag)
	}

	// Resolve configuration with precedence
	root, source, me, err := resolveEnvConfigWithSource(*rootFlag, *meFlag)
	if err != nil {
		return err
	}
	if !contextExplicit {
		if mismatch, checkErr := sessionPinMismatch(root); checkErr != nil {
			return checkErr
		} else if mismatch != nil {
			return ContextMismatchError("refusing env: %s. Use explicit --session <name> or --root <path> to repin", mismatch.Error())
		}
	}

	// Validate shell
	shell := strings.ToLower(strings.TrimSpace(*shellFlag))
	if !isValidShell(shell) {
		return UsageError("invalid shell %q (supported: sh, bash, zsh, fish)", shell)
	}

	// --session-name output mode: print session name and exit
	if *sessionNameFlag {
		if sessionNameOverride != "" {
			return writeStdout("%s\n", sessionNameOverride)
		}
		if name := inferredSessionIdentity(root); name != "" {
			return writeStdout("%s\n", name)
		}
		return nil // Not in a session — empty output, exit 0
	}

	// JSON output mode
	if *jsonFlag {
		baseRoot, sessionName, inSession := classifyEnvRoot(root)
		if sessionNameOverride != "" {
			baseRoot = sessionBaseRoot
			sessionName = sessionNameOverride
			inSession = true
		}
		project, peers := envProjectAndPeers(root)
		rootID, baseRootID := treeIdentityTokens(root, baseRoot)
		out := envOutput{
			SchemaVersion: 1,
			AMQVersion:    cliVersion,
			Root:          root,
			RootID:        rootID,
			BaseRoot:      baseRoot,
			BaseRootID:    baseRootID,
			SessionName:   sessionName,
			InSession:     inSession,
			Me:            me,
			Project:       project,
			RootSource:    string(source),
			Peers:         peers,
			Shell:         shell,
			Wake:          *wakeFlag,
		}
		return writeJSON(os.Stdout, out)
	}

	// Shell output pins this terminal to the resolved root, so emit it as an
	// absolute path: a relative export would re-resolve against every future
	// cwd, silently splitting one session name across per-directory trees.
	root, err = filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve absolute root for shell output: %w", err)
	}

	baseRoot, sessionName, inSession := classifyEnvRoot(root)
	if sessionNameOverride != "" {
		baseRoot = sessionBaseRoot
		sessionName = sessionNameOverride
		inSession = true
	}
	if baseRoot != "" {
		if baseRoot, err = filepath.Abs(baseRoot); err != nil {
			return fmt.Errorf("resolve absolute base root for shell output: %w", err)
		}
	}
	rootID, baseRootID := treeIdentityTokens(root, baseRoot)
	if err := writeShellEnv(root, baseRoot, rootID, baseRootID, sessionName, me, shell, *wakeFlag); err != nil {
		return err
	}
	if *exportFlag {
		return writeEnvExportPinNote(root, baseRoot, sessionName, inSession)
	}
	return nil
}

func classifyEnvRoot(root string) (baseRoot, sessionNameOut string, inSession bool) {
	base := classifyRoot(root)
	if base != "" && absPath(resolveRoot(root)) != absPath(resolveRoot(base)) {
		if session := inferredSessionIdentity(root); session != "" {
			return base, session, true
		}
	}
	return root, "", false
}

func inferredSessionIdentity(root string) string {
	session := resolveSessionName(root)
	if session == "" || validateSessionName(session) != nil {
		return ""
	}
	return session
}

func envProjectAndPeers(root string) (string, map[string]string) {
	peers := map[string]string{}

	// Include project identity and peer config from .amqrc so agents can
	// discover cross-project routing without reading .amqrc directly.
	rcResult, rcErr := findAmqrcForRoot(root)
	if rcErr != nil {
		return "", peers
	}

	project := projectFromAmqrcResult(rcResult)
	for name, path := range rcResult.Config.Peers {
		resolved, err := resolvePeerPath(rcResult, path)
		if err != nil {
			peers[name] = path
			continue
		}
		peers[name] = resolved
	}
	return project, peers
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
	var globalRCErr error
	globalResult, err := loadGlobalAmqrc()
	if err == nil && globalResult.Config.Root != "" {
		globalRCRoot = globalResult.Config.Root
		if !filepath.IsAbs(globalRCRoot) {
			globalRCRoot = filepath.Join(globalResult.Dir, globalRCRoot)
		}
	} else if err != nil && !errors.Is(err, errAmqrcNotFound) {
		globalRCErr = err
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
	if globalRCErr != nil {
		hasHigherPrecedenceRoot := rootFlag != "" || envRootVal != "" || rcRoot != "" || globalEnvRoot != ""
		if !hasHigherPrecedenceRoot {
			return "", "", "", globalRCErr
		}
		_ = writeStderr("warning: %v (using higher-precedence root)\n", globalRCErr)
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
	if err := validateAmqrcProvenance(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return amqrcResult{}, errAmqrcNotFound
		}
		return amqrcResult{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return amqrcResult{}, errAmqrcNotFound
	}
	if err := validateAmqrcFile(path); err != nil {
		return amqrcResult{}, err
	}
	var cfg amqrc
	if err := json.Unmarshal(data, &cfg); err != nil {
		return amqrcResult{}, fmt.Errorf("invalid ~/.amqrc: %w", err)
	}
	return amqrcResult{Config: cfg, Dir: home, Path: path}, nil
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
		// Refuse configuration whose provenance cannot be established. In
		// particular, symlinks and group/world-writable files are attacker
		// controlled in common shared-directory setups.
		if info, statErr := os.Lstat(rcPath); statErr == nil {
			if err := validateAmqrcInfo(rcPath, info); err != nil {
				return amqrcResult{}, err
			}
			if err := validateAmqrcFile(rcPath); err != nil {
				return amqrcResult{}, err
			}
		} else if !os.IsNotExist(statErr) {
			return amqrcResult{}, fmt.Errorf("cannot inspect .amqrc at %s: %w", rcPath, statErr)
		}
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
		return amqrcResult{Config: rc, Dir: dir, Path: rcPath}, nil
	}

	return amqrcResult{}, errAmqrcNotFound
}

func validateAmqrcProvenance(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validateAmqrcInfo(path, info)
}

func validateAmqrcInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing untrusted .amqrc at %s: symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing untrusted .amqrc at %s: not a regular file", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("refusing untrusted .amqrc at %s: group/world-writable mode %o", path, info.Mode().Perm())
	}
	return nil
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

func treeIdentityTokens(root, baseRoot string) (rootID, baseRootID string) {
	var rootErr, baseRootErr error
	rootID, rootErr = resolveTreeIdentityToken(root)
	baseRootID, baseRootErr = resolveTreeIdentityToken(baseRoot)
	if rootErr != nil || baseRootErr != nil {
		return "", ""
	}
	return rootID, baseRootID
}

func writeShellEnv(root, baseRoot, rootID, baseRootID, session, me, shell string, wake bool) error {
	switch shell {
	case "fish":
		return writeFishEnv(root, baseRoot, rootID, baseRootID, session, me, wake)
	default:
		return writePosixEnv(root, baseRoot, rootID, baseRootID, session, me, wake)
	}
}

func writePosixEnv(root, baseRoot, rootID, baseRootID, session, me string, wake bool) error {
	if root != "" {
		if err := writeStdout("export AM_ROOT=%s\n", shellQuotePosix(root)); err != nil {
			return err
		}
	}
	if baseRoot == "" {
		return fmt.Errorf("cannot emit AMQ context without an exact AM_BASE_ROOT")
	}
	if err := writeStdout("export AM_BASE_ROOT=%s\n", shellQuotePosix(baseRoot)); err != nil {
		return err
	}
	if rootID != "" {
		if err := writeStdout("export AM_ROOT_ID=%s\n", shellQuotePosix(rootID)); err != nil {
			return err
		}
	} else if err := writeStdoutLine("unset AM_ROOT_ID"); err != nil {
		return err
	}
	if baseRootID != "" {
		if err := writeStdout("export AM_BASE_ROOT_ID=%s\n", shellQuotePosix(baseRootID)); err != nil {
			return err
		}
	} else if err := writeStdoutLine("unset AM_BASE_ROOT_ID"); err != nil {
		return err
	}
	if err := writeStdout("export AM_SESSION=%s\n", shellQuotePosix(session)); err != nil {
		return err
	}
	if me != "" {
		if err := writeStdout("export AM_ME=%s\n", shellQuotePosix(me)); err != nil {
			return err
		}
	} else if err := writeStdoutLine("unset AM_ME"); err != nil {
		return err
	}
	if wake {
		if err := writeStdoutLine("amq wake &"); err != nil {
			return err
		}
	}
	return nil
}

func writeFishEnv(root, baseRoot, rootID, baseRootID, session, me string, wake bool) error {
	if root != "" {
		if err := writeStdout("set -gx AM_ROOT %s\n", shellQuoteFish(root)); err != nil {
			return err
		}
	}
	if baseRoot == "" {
		return fmt.Errorf("cannot emit AMQ context without an exact AM_BASE_ROOT")
	}
	if err := writeStdout("set -gx AM_BASE_ROOT %s\n", shellQuoteFish(baseRoot)); err != nil {
		return err
	}
	if rootID != "" {
		if err := writeStdout("set -gx AM_ROOT_ID %s\n", shellQuoteFish(rootID)); err != nil {
			return err
		}
	} else if err := writeStdoutLine("set -e AM_ROOT_ID"); err != nil {
		return err
	}
	if baseRootID != "" {
		if err := writeStdout("set -gx AM_BASE_ROOT_ID %s\n", shellQuoteFish(baseRootID)); err != nil {
			return err
		}
	} else if err := writeStdoutLine("set -e AM_BASE_ROOT_ID"); err != nil {
		return err
	}
	if session == "" {
		if err := writeStdoutLine("set -gx AM_SESSION ''"); err != nil {
			return err
		}
	} else if err := writeStdout("set -gx AM_SESSION %s\n", shellQuoteFish(session)); err != nil {
		return err
	}
	if me != "" {
		if err := writeStdout("set -gx AM_ME %s\n", shellQuoteFish(me)); err != nil {
			return err
		}
	} else if err := writeStdoutLine("set -e AM_ME"); err != nil {
		return err
	}
	if wake {
		if err := writeStdoutLine("amq wake &"); err != nil {
			return err
		}
	}
	return nil
}

func writeEnvExportPinNote(root, baseRoot, session string, inSession bool) error {
	if inSession {
		return writeStderr("note: pinned to AMQ session %s; use one terminal, one session (AM_ROOT=%s, AM_BASE_ROOT=%s)\n", session, root, baseRoot)
	}
	return writeStderr("note: pinned to AMQ root %s\n", root)
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
	escaped := strings.ReplaceAll(s, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "'", "\\'")
	return "'" + escaped + "'"
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
// When root is provided (non-empty), lookup is scoped to root's ancestors only.
// This ensures --root / AM_ROOT fully determines which project config is used,
// even when cwd is inside a different project.
func findAmqrcForRoot(root string) (amqrcResult, error) {
	if root != "" {
		absRoot, absErr := filepath.Abs(root)
		if absErr != nil {
			return amqrcResult{}, absErr
		}
		dir := absRoot
		for {
			rcPath := filepath.Join(dir, ".amqrc")
			if info, statErr := os.Lstat(rcPath); statErr == nil {
				if err := validateAmqrcInfo(rcPath, info); err != nil {
					return amqrcResult{}, err
				}
				if validateErr := validateAmqrcFile(rcPath); validateErr != nil {
					return amqrcResult{}, validateErr
				}
			}
			data, readErr := os.ReadFile(rcPath)
			if readErr == nil {
				var rc amqrc
				if jsonErr := json.Unmarshal(data, &rc); jsonErr != nil {
					return amqrcResult{}, fmt.Errorf("invalid .amqrc at %s: %w", rcPath, jsonErr)
				}
				return amqrcResult{Config: rc, Dir: dir, Path: rcPath}, nil
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
		return amqrcResult{}, errAmqrcNotFound
	}
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
	abs, err := resolvePeerPath(result, peerPath)
	if err != nil {
		return "", fmt.Errorf("resolve peer path for %q: %w", project, err)
	}
	return abs, nil
}

func resolvePeerPath(result amqrcResult, peerPath string) (string, error) {
	if !filepath.IsAbs(peerPath) {
		peerPath = filepath.Join(result.Dir, peerPath)
	}
	return filepath.Abs(peerPath)
}

func projectFromAmqrcResult(result amqrcResult) string {
	if result.Config.Project != "" {
		return result.Config.Project
	}
	// Only infer project from directory basename for project-local .amqrc,
	// not global ~/.amqrc (which is a queue locator, not a project identity).
	home, _ := os.UserHomeDir()
	if home != "" && result.Dir == home {
		return ""
	}
	return filepath.Base(result.Dir)
}

// resolveProject returns the project name for the current .amqrc.
// Uses the explicit "project" field if set, otherwise falls back to the
// basename of the directory containing .amqrc.
func resolveProject(root string) string {
	result, err := findAmqrcForRoot(root)
	if err != nil {
		return ""
	}
	return projectFromAmqrcResult(result)
}
