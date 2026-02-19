package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type commonFlags struct {
	Root    string
	Me      string
	JSON    bool
	Strict  bool
	rootSet bool // true when --root was explicitly provided
	flagSet *flag.FlagSet
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	flags := &commonFlags{flagSet: fs}
	fs.StringVar(&flags.Root, "root", defaultRoot(), "Root directory for the queue")
	fs.StringVar(&flags.Me, "me", defaultMe(), "Agent handle (or AM_ME)")
	fs.BoolVar(&flags.JSON, "json", false, "Emit JSON output")
	fs.BoolVar(&flags.Strict, "strict", false, "Error on unknown handles (default: warn)")
	return flags
}

// validate checks routing safety for commands that use commonFlags.
//  1. If --root was explicitly provided, guards against AM_ROOT conflicts.
//  2. If --root was NOT provided, requires AM_ROOT to be set — falling back
//     to .amqrc alone is ambiguous because .amqrc can change between commands.
//
// Call after flag parsing in every command that uses commonFlags.
func (f *commonFlags) validate() error {
	// Check if --root was explicitly set by the user.
	if f.flagSet != nil {
		f.flagSet.Visit(func(fl *flag.Flag) {
			if fl.Name == "root" {
				f.rootSet = true
			}
		})
	}
	if !f.rootSet {
		// root was defaulted (not from --root flag).
		// Require AM_ROOT to be set — .amqrc fallback is ambiguous.
		if os.Getenv(envRoot) == "" {
			me := defaultMe()
			hint := "<agent>"
			if me != "" {
				hint = me
			}
			return fmt.Errorf(
				"AM_ROOT is not set — session routing is ambiguous\n\n"+
					"  .amqrc can change between commands, causing silent misrouting.\n"+
					"  Fix: initialize your session first:\n\n"+
					"    eval \"$(amq env --me %s)\"     # pin AM_ROOT for this shell\n"+
					"    amq coop exec %s               # full co-op setup\n"+
					"    amq send --root <path> ...      # explicit per-command", hint, hint)
		}
		return nil // AM_ROOT is set, no conflict possible
	}
	return guardRootOverride(f.Root)
}

// sessionName extracts the session name (last path component) from a resolved root path.
func sessionName(root string) string { return filepath.Base(root) }

// cachedAmqrcRoot returns the resolved queue root from .amqrc (base/session), cached via sync.Once.
// Returns "" on any error (best-effort for defaulting, not validation).
var amqrcOnce sync.Once
var amqrcCachedRoot string

func cachedAmqrcRoot() string {
	amqrcOnce.Do(func() {
		result, err := findAndLoadAmqrc()
		if err != nil {
			return
		}
		base := result.Config.Root
		if base == "" {
			return
		}
		if !filepath.IsAbs(base) {
			base = filepath.Join(result.Dir, base)
		}
		session := result.Config.DefaultSession
		if session == "" {
			session = defaultSessionName
		}
		root := filepath.Join(base, session)
		warnLegacyLayout(base, root)
		amqrcCachedRoot = root
	})
	return amqrcCachedRoot
}

// warnLegacyLayout checks if the base directory has a legacy flat layout
// (agents/ directly under base instead of under a session subdirectory).
// Prints a warning to stderr if detected.
func warnLegacyLayout(base, sessionRoot string) {
	if dirExists(sessionRoot) {
		return // Session root exists, nothing to warn about
	}
	// Check if base has agents/ directly (legacy flat layout)
	if dirExists(filepath.Join(base, "agents")) {
		_ = writeStderr("warning: legacy layout detected — %s has agents/ directly instead of under %s\n", base, filepath.Base(sessionRoot))
		_ = writeStderr("  Run 'amq coop init --force' to create the new session layout, then move your data.\n")
	}
}

// resetAmqrcCache resets the sync.Once for testing.
// Test-only; not safe for parallel tests.
func resetAmqrcCache() {
	amqrcOnce = sync.Once{}
	amqrcCachedRoot = ""
}

func defaultRoot() string {
	if env := strings.TrimSpace(os.Getenv(envRoot)); env != "" {
		return env
	}
	if root := cachedAmqrcRoot(); root != "" {
		return root
	}
	fallback := filepath.Join(".agent-mail", defaultSessionName)
	// Warn if legacy flat layout exists without .amqrc
	warnLegacyLayout(".agent-mail", fallback)
	return fallback
}

func resolveRoot(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	cleaned := filepath.Clean(raw)
	if filepath.IsAbs(cleaned) {
		return cleaned
	}
	cwd, err := os.Getwd()
	if err != nil {
		return cleaned
	}
	candidate := filepath.Join(cwd, cleaned)
	if dirExists(candidate) {
		return absPath(candidate)
	}
	if found, ok := findRootInParents(cwd, cleaned); ok {
		return found
	}
	return cleaned
}

// guardRootOverride checks whether the user is trying to override AM_ROOT
// via --root flag. Returns an error if AM_ROOT is set in the env and the
// resolved root differs. This prevents agents from accidentally using a
// different root than their session.
func guardRootOverride(flagRoot string) error {
	envVal := strings.TrimSpace(os.Getenv(envRoot))
	if envVal == "" || flagRoot == "" {
		return nil // No conflict possible
	}
	// Compare resolved paths
	envResolved := resolveRoot(envVal)
	flagResolved := resolveRoot(flagRoot)
	if envResolved == flagResolved {
		return nil // Same path, no conflict
	}
	return fmt.Errorf("AM_ROOT is set to %q but --root specifies %q; remove --root to use the session root, or unset AM_ROOT to override", envVal, flagRoot)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func findRootInParents(startDir, relative string) (string, bool) {
	dir := startDir
	for {
		candidate := filepath.Join(dir, relative)
		if dirExists(candidate) {
			return absPath(candidate), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

func defaultMe() string {
	if env := strings.TrimSpace(os.Getenv(envMe)); env != "" {
		return env
	}
	return ""
}

func requireMe(handle string) error {
	if strings.TrimSpace(handle) == "" {
		return UsageError("--me is required (or set AM_ME, e.g., export AM_ME=your-handle)")
	}
	return nil
}

// loadKnownAgents loads the agent list from config.json.
// Returns nil slice if config doesn't exist.
// If strict=true, returns an error for unreadable/corrupt config; otherwise warns to stderr.
func loadKnownAgents(root string, strict bool) ([]string, error) {
	configPath := filepath.Join(root, "meta", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No config, no validation
		}
		// Config exists but unreadable
		msg := fmt.Sprintf("cannot read config.json: %v", err)
		if strict {
			return nil, errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil, nil
	}

	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Config exists but invalid JSON
		msg := fmt.Sprintf("invalid config.json: %v", err)
		if strict {
			return nil, errors.New(msg)
		}
		_ = writeStderr("warning: %s\n", msg)
		return nil, nil
	}

	return cfg.Agents, nil
}

func loadKnownAgentSet(root string, strict bool) (map[string]struct{}, error) {
	agents, err := loadKnownAgents(root, strict)
	if err != nil || len(agents) == 0 {
		return nil, err
	}
	known := make(map[string]struct{}, len(agents))
	for _, agent := range agents {
		known[agent] = struct{}{}
	}
	return known, nil
}

// validateKnownHandles validates handles against config.json.
// Accepts variadic handles for convenience (single or multiple).
// Returns nil if config doesn't exist or all handles are known.
// If strict=true, returns an error for unknown handles or unreadable/corrupt config; otherwise warns to stderr.
func validateKnownHandles(root string, strict bool, handles ...string) error {
	agents, err := loadKnownAgents(root, strict)
	if err != nil {
		return err
	}
	if agents == nil {
		return nil // No config, no validation
	}

	known := make(map[string]bool, len(agents))
	for _, a := range agents {
		known[a] = true
	}

	var unknown []string
	for _, h := range handles {
		if !known[h] {
			unknown = append(unknown, h)
		}
	}

	if len(unknown) == 0 {
		return nil
	}

	var msg string
	if len(unknown) == 1 {
		msg = fmt.Sprintf("handle %q not in config.json agents %v", unknown[0], agents)
	} else {
		msg = fmt.Sprintf("unknown handles %v (known: %v)", unknown, agents)
	}
	if strict {
		return errors.New(msg)
	}
	_ = writeStderr("warning: %s\n", msg)
	return nil
}

func normalizeHandle(raw string) (string, error) {
	handle := strings.TrimSpace(raw)
	if handle == "" {
		return "", errors.New("agent handle cannot be empty")
	}
	if strings.ContainsAny(handle, "/\\") {
		return "", fmt.Errorf("invalid handle (slashes not allowed): %s", handle)
	}
	if handle != strings.ToLower(handle) {
		return "", fmt.Errorf("handle must be lowercase: %s", handle)
	}
	for _, r := range handle {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' {
			continue
		}
		return "", fmt.Errorf("invalid handle (allowed: a-z, 0-9, -, _): %s", handle)
	}
	return handle, nil
}

// validateSessionName checks that a session name uses safe characters for
// directory names. Allows lowercase letters, digits, hyphens, and underscores
// (same charset as handles).
func validateSessionName(name string) error {
	if name == "" {
		return UsageError("session name cannot be empty")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return UsageError("invalid session name %q (allowed: a-z, 0-9, -, _)", name)
	}
	return nil
}

func parseHandles(raw string) ([]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		handle, err := normalizeHandle(part)
		if err != nil {
			return nil, err
		}
		out = append(out, handle)
	}
	return out, nil
}

func splitRecipients(raw string) ([]string, error) {
	out, err := parseHandles(raw)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, UsageError("--to is required")
	}
	return out, nil
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func splitList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func readBody(bodyFlag string) (string, error) {
	if bodyFlag == "" || bodyFlag == "@-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if strings.HasPrefix(bodyFlag, "@") {
		path := strings.TrimPrefix(bodyFlag, "@")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return bodyFlag, nil
}

func isHelp(arg string) bool {
	switch arg {
	case "-h", "--help", "help":
		return true
	default:
		return false
	}
}

func parseFlags(fs *flag.FlagSet, args []string, usage func()) (bool, error) {
	fs.SetOutput(io.Discard)
	if usage != nil {
		fs.Usage = usage
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func usageWithFlags(fs *flag.FlagSet, usage string, notes ...string) func() {
	return func() {
		_ = writeStdoutLine("Usage:")
		_ = writeStdoutLine("  " + usage)
		if len(notes) > 0 {
			_ = writeStdoutLine("")
			for _, note := range notes {
				_ = writeStdoutLine(note)
			}
		}
		_ = writeStdoutLine("")
		_ = writeStdoutLine("Options:")
		_ = writeFlagDefaults(fs)
	}
}

func writeFlagDefaults(fs *flag.FlagSet) error {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	if buf.Len() == 0 {
		return nil
	}
	return writeStdout("%s", buf.String())
}

func confirmPrompt(prompt string) (bool, error) {
	return doConfirmPrompt(prompt, false)
}

// confirmPromptYes is like confirmPrompt but defaults to Yes on empty input.
func confirmPromptYes(prompt string) (bool, error) {
	return doConfirmPrompt(prompt, true)
}

func doConfirmPrompt(prompt string, defaultYes bool) (bool, error) {
	hint := "[y/N]"
	if defaultYes {
		hint = "[Y/n]"
	}
	if err := writeStdout("%s %s: ", prompt, hint); err != nil {
		return false, err
	}

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return defaultYes, nil
		}
		return false, err
	}

	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return defaultYes, nil
	}
	return line == "y" || line == "yes", nil
}

func ensureFilename(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("message id is required")
	}
	if strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if id == "." || id == ".." {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if filepath.Base(id) != id {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if strings.ContainsAny(id, "/\\") {
		return "", fmt.Errorf("invalid message id: %s", id)
	}
	if !strings.HasSuffix(id, ".md") {
		id += ".md"
	}
	return id, nil
}

func ensureSafeBaseName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name cannot be empty")
	}
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	if filepath.Base(name) != name {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	if strings.HasSuffix(name, ".md") {
		return "", fmt.Errorf("invalid name: %s", name)
	}
	return name, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeStdout(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stdout, format, args...)
	return err
}

func writeStdoutLine(args ...any) error {
	_, err := fmt.Fprintln(os.Stdout, args...)
	return err
}

func writeStderr(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stderr, format, args...)
	return err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// multiStringFlag allows a flag to be specified multiple times.
// Implements flag.Value interface.
type multiStringFlag []string

func (m *multiStringFlag) String() string {
	if m == nil {
		return ""
	}
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

// parseContext parses a context JSON string or @file.json.
func parseContext(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var data []byte
	if strings.HasPrefix(raw, "@") {
		path := strings.TrimPrefix(raw, "@")
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read context file: %w", err)
		}
	} else {
		data = []byte(raw)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse context JSON: %w", err)
	}
	return result, nil
}

// ensureGitignore adds the root directory and .amqrc to .gitignore, creating the file if needed.
// Returns true if the file was created or updated.
// Skips absolute paths since they don't make sense in .gitignore.
func ensureGitignore(root string) bool {
	// Skip absolute paths - they don't belong in .gitignore
	if filepath.IsAbs(root) {
		return false
	}

	gitignorePath := ".gitignore"

	// Normalize root for gitignore (add trailing slash for directory)
	rootPattern := root
	if !strings.HasSuffix(rootPattern, "/") {
		rootPattern += "/"
	}

	// Patterns to ensure are in .gitignore
	patterns := []string{rootPattern, ".amqrc"}

	// Read existing content (may not exist)
	data, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return false
	}

	// Check which patterns are missing
	var missing []string
	lines := strings.Split(string(data), "\n")
	for _, pattern := range patterns {
		found := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			// Check various forms the pattern might appear
			if trimmed == pattern || trimmed == "/"+pattern || trimmed == strings.TrimSuffix(pattern, "/") {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, pattern)
		}
	}

	if len(missing) == 0 {
		return false // All patterns already present
	}

	// Append missing patterns to file (or create new)
	toAppend := "# Agent Message Queue\n" + strings.Join(missing, "\n") + "\n"
	if len(data) > 0 {
		// Ensure we start on a new line
		if !strings.HasSuffix(string(data), "\n") {
			toAppend = "\n" + toAppend
		} else {
			toAppend = "\n" + toAppend
		}
	}

	if err := os.WriteFile(gitignorePath, append(data, []byte(toAppend)...), 0644); err != nil {
		return false
	}
	return true
}
