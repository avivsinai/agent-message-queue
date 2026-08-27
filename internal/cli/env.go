package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/sessionguard"
)

// amqrc represents the .amqrc configuration file format.
// Root is the literal queue root directory (e.g., ".agent-mail").
// Agent identity ('me') should be set per-terminal via AM_ME env var or --me flag.
type amqrc struct {
	Root    string            `json:"root"`
	Project string            `json:"project,omitempty"` // explicit project name (defaults to directory basename)
	Peers   map[string]string `json:"peers,omitempty"`   // peer name → peer's base root path
}

var (
	amqrcLstat          = os.Lstat
	globalAmqrcReadFile = os.ReadFile
	gitMarkerLstat      = os.Lstat
)

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
		"  Root: flags > env (AM_ROOT) > project .amqrc > AMQ_GLOBAL_ROOT > implicit fallbacks",
		"  Inside a Git worktree or bare repository: repo-local .agent-mail; ~/.amqrc is ineligible",
		"  Outside Git: ~/.amqrc > detected .agent-mail",
		"  Me:   flags > env (AM_ME)",
		"",
		"Note: .amqrc only configures 'root'. Agent identity ('me') is set",
		"per-terminal via --me or AM_ME, since different terminals may use",
		"different agents on the same project.",
		"",
		"Examples:",
		"  amq_context=\"$(amq env --me claude)\" && eval \"$amq_context\"",
		"  amq_context=\"$(amq env --session feature-x --me claude --export)\" && eval \"$amq_context\"",
		"  amq_context=\"$(amq env --me codex --wake)\" && eval \"$amq_context\"",
		"  amq_context=\"$(amq env --session feature-x --me claude)\" && eval \"$amq_context\"",
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
			if err := validateLegacySessionPinRoot(pin); err != nil {
				return err
			}
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
	if contextExplicit {
		decision := sessionguard.Decide(sessionguard.Input{
			Kind: sessionguard.KindEnv,
			Pin:  sessionguard.PinAbsent, Relation: sessionguard.TargetUnbound,
			Flags: sessionguard.Flags{ExplicitContext: true},
		})
		if decision.Verdict != sessionguard.Allow {
			return ContextMismatchError("refusing env: explicit context replacement was not authorized")
		}
	} else {
		pin, pinErr := loadSessionPin()
		if pinErr != nil {
			decision := sessionGuardDecisionForContext(
				sessionguard.KindEnv,
				sessionguard.ChannelExit5,
				sessionguard.PinInvalid,
				&SessionContextError{Message: pinErr.Error()},
				sessionguard.Flags{},
			)
			if decision.Verdict == sessionguard.Allow {
				return nil
			}
			return pinErr
		}
		mismatch, checkErr := sessionPinMismatchWithPin(root, pin)
		decision := sessionGuardDecisionForContext(
			sessionguard.KindEnv,
			sessionguard.ChannelExit5,
			sessionGuardPinStateFor(pin),
			mismatch,
			sessionguard.Flags{},
		)
		if checkErr != nil {
			return checkErr
		} else if decision.Verdict != sessionguard.Allow && mismatch != nil {
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
//   - Root: flags > env > project .amqrc > AMQ_GLOBAL_ROOT > implicit fallbacks
//   - Inside a Git worktree or bare repository: repo-local .agent-mail; ~/.amqrc is ineligible
//   - Outside Git: ~/.amqrc > auto-detect
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

	// 3. Establish whether home configuration is eligible. This uses only
	// filesystem evidence and ignores GIT_* environment variables.
	gitTop, insideGit := gitWorktreeRootFromCWD()

	// 4. Try global ~/.amqrc outside Git worktrees only.
	var globalRCRoot string
	var globalRCErr error
	if !insideGit {
		globalResult, err := loadGlobalAmqrc()
		if err == nil && globalResult.Config.Root != "" {
			globalRCRoot = globalResult.Config.Root
			if !filepath.IsAbs(globalRCRoot) {
				globalRCRoot = filepath.Join(globalResult.Dir, globalRCRoot)
			}
		} else if err != nil && !errors.Is(err, errAmqrcNotFound) {
			globalRCErr = err
		}
	}

	// 5. Auto-detect .agent-mail/ directory
	autoRoot := detectAgentMailDir()

	// 6. Environment variables
	envRootVal := strings.TrimSpace(os.Getenv(envRoot))
	envMeVal := strings.TrimSpace(os.Getenv(envMe))

	// 7. Command-line flags (already have rootFlag, meFlag)

	// AMQ_GLOBAL_ROOT is explicit routing authority. Home configuration is only
	// an implicit convenience outside Git worktrees.
	switch {
	case rootFlag != "":
		root, source = rootFlag, rootSourceFlag
	case envRootVal != "":
		root, source = envRootVal, rootSourceEnv
	case rcRoot != "":
		root, source = rcRoot, rootSourceProjectRC
	case globalEnvRoot != "":
		root, source = globalEnvRoot, rootSourceGlobalEnv
	case insideGit && autoRoot != "":
		root, source = autoRoot, rootSourceAutoDetect
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
		if insideGit {
			return "", "", "", noEligibleRootInGitError(gitTop)
		}
		return "", "", "", fmt.Errorf("cannot determine root: no .amqrc found, no .agent-mail/ directory, AM_ROOT not set, and no global config (~/.amqrc or AMQ_GLOBAL_ROOT)")
	}

	// Canonicalize the winning root before pin validation, JSON/shell output,
	// doctor inspection, or route planning. Participating commands use the same
	// ancestor-aware resolver; returning a raw relative value here could
	// authorize one existing parent queue but export a different cwd-relative
	// path.
	root = absPath(resolveRoot(root))

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
	data, err := globalAmqrcReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return amqrcResult{}, errAmqrcNotFound
		}
		return amqrcResult{}, fmt.Errorf("cannot read ~/.amqrc at %s: %w", path, err)
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

// resolveDiscoveredBaseRoot returns the first configured or detected base root.
// The boolean is false only when no project, repo-local, or global source exists.
func resolveDiscoveredBaseRoot() (string, bool, error) {
	// 1. Project .amqrc
	result, err := findAndLoadAmqrc()
	if err == nil && result.Config.Root != "" {
		base := result.Config.Root
		if !filepath.IsAbs(base) {
			base = filepath.Join(result.Dir, base)
		}
		return base, true, nil
	}
	if err != nil && !errors.Is(err, errAmqrcNotFound) {
		return "", false, err
	}

	// 2. Explicit global root.
	if globalEnv := strings.TrimSpace(os.Getenv(envGlobalRoot)); globalEnv != "" {
		return globalEnv, true, nil
	}

	// 3. Inside Git, only the repo-local default remains eligible.
	gitTop, insideGit := gitWorktreeRootFromCWD()
	if insideGit {
		if local := detectAgentMailDir(); local != "" {
			return local, true, nil
		}
		return "", false, noEligibleRootInGitError(gitTop)
	}

	// 4. Outside Git, preserve the historical ~/.amqrc-before-auto-detect
	// fallback ordering.
	globalResult, err := loadGlobalAmqrc()
	if err == nil && globalResult.Config.Root != "" {
		base := globalResult.Config.Root
		if !filepath.IsAbs(base) {
			base = filepath.Join(globalResult.Dir, base)
		}
		return base, true, nil
	}
	if err != nil && !errors.Is(err, errAmqrcNotFound) {
		return "", false, err
	}

	// 5. Non-repository local layout.
	if local := detectAgentMailDir(); local != "" {
		return local, true, nil
	}

	return "", false, nil
}

// resolveDiscoveredBaseRootForBootstrap preserves ordinary root precedence but
// turns an unconfigured Git worktree into a bootstrap target. Participating
// commands must continue using resolveDiscoveredBaseRoot so they never create a
// queue merely because cwd is inside a repository.
func resolveDiscoveredBaseRootForBootstrap() (string, bool, error) {
	base, found, err := resolveDiscoveredBaseRoot()
	if err == nil || GetExitCode(err) != ExitContextMismatch {
		return base, found, err
	}

	if top, ok := gitWorktreeTopFromCWD(); ok {
		return filepath.Join(top, defaultCoopRoot), false, nil
	}
	return "", false, err
}

func noEligibleRootInGitError(top string) error {
	return ContextMismatchError(
		"cannot determine an eligible AMQ root while cwd is inside Git worktree or bare repository %s: no project .amqrc, repo-local .agent-mail, or explicit root was found; implicit ~/.amqrc is ineligible here; --session selects a session but does not authorize a base root; run 'amq coop init' (or 'amq coop exec <agent>') in this repository to create its queue; for a bare repository, cd into a worktree or pass --root explicitly; alternatively configure this repository, set AMQ_GLOBAL_ROOT or AM_ROOT, or pass --root explicitly",
		top,
	)
}

func gitWorktreeTopFromCWD() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
		cwd = resolved
	}
	top, err := gitTopLevel(cwd)
	if err != nil {
		return "", false
	}
	markerInfo, err := gitMarkerLstat(filepath.Join(top, ".git"))
	if err != nil || (!markerInfo.IsDir() && !markerInfo.Mode().IsRegular()) {
		return "", false
	}
	boundary, insideGit := gitWorktreeRootFromCWD()
	if !insideGit || !sameTreeIdentity(top, boundary) {
		return "", false
	}
	return top, true
}

func gitWorktreeRootFromCWD() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	return gitWorktreeRootFrom(cwd)
}

// gitWorktreeRootFrom returns the innermost Git worktree or bare-repository
// boundary that contains path. A nested or linked worktree is its own
// ceiling, so a parent checkout's .amqrc or .agent-mail is not inferred.
func gitWorktreeRootFrom(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	path = abs
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	}
	bareCandidate := ""
	for dir := path; ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, ".git")
		_, statErr := gitMarkerLstat(marker)
		if statErr == nil {
			return dir, true
		}
		if !os.IsNotExist(statErr) {
			return dir, true
		}
		if bareCandidate == "" && bareGitRepositorySignal(dir) {
			bareCandidate = dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			if bareCandidate != "" {
				return bareCandidate, true
			}
			return "", false
		}
	}
}

func bareGitRepositorySignal(dir string) bool {
	// A worktree's .git directory has the same internal shape as a bare
	// repository, but it is not the routing boundary. Continue upward so the
	// parent worktree's .git marker wins.
	if filepath.Base(dir) == ".git" {
		return false
	}

	head := bareGitHEADSignal(filepath.Join(dir, "HEAD"))
	objects := bareGitTypedMarkerSignal(filepath.Join(dir, "objects"), true)
	refs := bareGitReferenceStoreSignal(dir)

	// Absence or a wrong marker type disproves the candidate. Once the other
	// repository structure is present, an inspection failure remains a safety
	// signal so permission errors cannot re-enable an implicit home route.
	return head != bareGitMarkerAbsent &&
		objects != bareGitMarkerAbsent &&
		refs != bareGitMarkerAbsent
}

type bareGitMarkerSignal uint8

const (
	bareGitMarkerAbsent bareGitMarkerSignal = iota
	bareGitMarkerPresent
	bareGitMarkerUncertain
)

func bareGitHEADSignal(path string) bareGitMarkerSignal {
	info, err := gitMarkerLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bareGitMarkerAbsent
		}
		return bareGitMarkerUncertain
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return bareGitMarkerAbsent
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return bareGitMarkerUncertain
	}
	head := strings.TrimSpace(string(data))
	if strings.HasPrefix(head, "ref: ") {
		ref := strings.TrimSpace(strings.TrimPrefix(head, "ref: "))
		if ref != "" && !strings.ContainsAny(ref, " \t\r\n") {
			return bareGitMarkerPresent
		}
		return bareGitMarkerAbsent
	}
	if len(head) != 40 && len(head) != 64 {
		return bareGitMarkerAbsent
	}
	for _, char := range head {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return bareGitMarkerAbsent
		}
	}
	return bareGitMarkerPresent
}

func bareGitTypedMarkerSignal(path string, directory bool) bareGitMarkerSignal {
	info, err := gitMarkerLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bareGitMarkerAbsent
		}
		return bareGitMarkerUncertain
	}
	if directory {
		if info.IsDir() {
			return bareGitMarkerPresent
		}
		return bareGitMarkerAbsent
	}
	if info.Mode().IsRegular() {
		return bareGitMarkerPresent
	}
	return bareGitMarkerAbsent
}

func bareGitReferenceStoreSignal(dir string) bareGitMarkerSignal {
	markers := []struct {
		name      string
		directory bool
	}{
		{name: "refs", directory: true},
		{name: "packed-refs"},
		{name: "reftable", directory: true},
	}
	uncertain := false
	for _, marker := range markers {
		switch bareGitTypedMarkerSignal(filepath.Join(dir, marker.name), marker.directory) {
		case bareGitMarkerPresent:
			return bareGitMarkerPresent
		case bareGitMarkerUncertain:
			uncertain = true
		}
	}
	if uncertain {
		return bareGitMarkerUncertain
	}
	return bareGitMarkerAbsent
}

// resolveBaseRoot returns the base root directory (without session suffix).
// Tries project .amqrc first, then explicit AMQ_GLOBAL_ROOT, then
// context-eligible implicit fallbacks, then defaultCoopRoot.
func resolveBaseRoot() (string, error) {
	base, found, err := resolveDiscoveredBaseRoot()
	if err != nil {
		return "", err
	}
	if found {
		return base, nil
	}
	return defaultCoopRoot, nil
}

// findAndLoadAmqrc searches for .amqrc in current and parent directories.
// Returns errAmqrcNotFound if no .amqrc exists (non-fatal).
// Returns other errors for invalid/unreadable .amqrc (fatal).
func findAndLoadAmqrc() (amqrcResult, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return amqrcResult{}, err
	}
	ceiling := ""
	if top, insideGit := gitWorktreeRootFromCWD(); insideGit {
		ceiling = top
		if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
			cwd = resolved
		}
	}

	dir := cwd
	// ~/.amqrc is a global fallback, not a project config. Loading it here
	// mislabeled the global queue as project-local and let it shadow a repo's
	// own .agent-mail. When HOME is itself the Git worktree root, however, the
	// file is worktree-local authority and the inclusive Git ceiling wins.
	for ceiling != "" || !isHomeConfigDir(dir) {
		rcPath := filepath.Join(dir, ".amqrc")
		// Refuse configuration whose provenance cannot be established. In
		// particular, symlinks and group/world-writable files are attacker
		// controlled in common shared-directory setups.
		if info, statErr := amqrcLstat(rcPath); statErr == nil {
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
				if ceiling != "" && sameCleanPath(dir, ceiling) {
					break
				}
				// Keep searching in eligible parent directories.
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
	return nil
}

// detectAgentMailDir searches for .agent-mail/ in current and parent directories.
func detectAgentMailDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	ceiling := ""
	if top, insideGit := gitWorktreeRootFromCWD(); insideGit {
		ceiling = top
		if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
			cwd = resolved
		}
	}

	dir := cwd
	// A home-level .agent-mail is global state, not evidence that the current
	// repository owns that queue. If HOME is the Git worktree root, it is local
	// evidence and must be inspected at the inclusive ceiling.
	for ceiling != "" || !isHomeConfigDir(dir) {
		candidate := filepath.Join(dir, ".agent-mail")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			// Return relative path if in cwd, absolute otherwise
			if dir == cwd {
				return ".agent-mail"
			}
			return candidate
		}
		if ceiling != "" && sameCleanPath(dir, ceiling) {
			break
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return ""
}

func sameCleanPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func isHomeConfigDir(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return false
	}
	if dirInfo, dirErr := os.Stat(dir); dirErr == nil {
		if homeInfo, homeErr := os.Stat(home); homeErr == nil {
			return os.SameFile(dirInfo, homeInfo)
		}
	}
	if resolvedDir, dirErr := filepath.EvalSymlinks(dir); dirErr == nil {
		dir = resolvedDir
	}
	if resolvedHome, homeErr := filepath.EvalSymlinks(home); homeErr == nil {
		home = resolvedHome
	}
	absDir, dirErr := filepath.Abs(dir)
	absHome, homeErr := filepath.Abs(home)
	if dirErr != nil || homeErr != nil {
		return filepath.Clean(dir) == filepath.Clean(home)
	}
	return filepath.Clean(absDir) == filepath.Clean(absHome)
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
// When root is provided (non-empty), project lookup is scoped to root's
// ancestors. The global ~/.amqrc is eligible only when it configured that exact
// base root or one of its direct sessions.
func findAmqrcForRoot(root string) (amqrcResult, error) {
	if root != "" {
		absRoot, absErr := filepath.Abs(root)
		if absErr != nil {
			return amqrcResult{}, absErr
		}
		ceiling := ""
		if top, insideGit := gitWorktreeRootFrom(absRoot); insideGit {
			ceiling = top
		}
		dir := absRoot
		for ceiling != "" || !isHomeConfigDir(dir) {
			rcPath := filepath.Join(dir, ".amqrc")
			if info, statErr := amqrcLstat(rcPath); statErr == nil {
				if err := validateAmqrcInfo(rcPath, info); err != nil {
					return amqrcResult{}, err
				}
				if validateErr := validateAmqrcFile(rcPath); validateErr != nil {
					return amqrcResult{}, validateErr
				}
			} else if !os.IsNotExist(statErr) {
				// Do not walk past an unreadable or otherwise unverifiable config.
				return amqrcResult{}, fmt.Errorf("cannot inspect .amqrc at %s: %w", rcPath, statErr)
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
			if ceiling != "" && sameCleanPath(dir, ceiling) {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		globalResult, globalErr := loadGlobalAmqrc()
		if globalErr == nil && strings.TrimSpace(globalResult.Config.Root) != "" {
			globalRoot := globalResult.Config.Root
			if !filepath.IsAbs(globalRoot) {
				globalRoot = filepath.Join(globalResult.Dir, globalRoot)
			}
			if isBaseOrSessionRoot(absRoot, globalRoot) {
				return globalResult, nil
			}
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
	return resolvePeerFromAmqrcResult(result, project)
}

func resolvePeerFromAmqrcResult(result amqrcResult, project string) (string, error) {
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
