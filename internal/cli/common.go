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
	flagSet *flag.FlagSet
}

const reservedHumanHandle = "user"

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	flags := &commonFlags{flagSet: fs}
	fs.StringVar(&flags.Root, "root", defaultRoot(), "Root directory for the queue")
	fs.StringVar(&flags.Me, "me", defaultMe(), "Agent handle (or AM_ME)")
	fs.BoolVar(&flags.JSON, "json", false, "Emit JSON output")
	fs.BoolVar(&flags.Strict, "strict", false, "Error on unknown handles (default: warn)")
	return flags
}

// rootExplicit reports whether --root was passed on the command line (as opposed
// to defaulted from env/.amqrc). Used to distinguish a deliberate root override
// from the resolved default.
func (f *commonFlags) rootExplicit() bool {
	return flagWasVisited(f.flagSet, "root")
}

// warnRootOverride emits a diagnostic note to stderr when --root was explicitly
// provided and differs from AM_ROOT. This helps users notice when a command
// operates on a different root than their session. Not an error — --root wins.
func (f *commonFlags) warnRootOverride() {
	if !f.rootExplicit() {
		return
	}
	envVal := strings.TrimSpace(os.Getenv(envRoot))
	if envVal == "" {
		return
	}
	if resolveRoot(envVal) != resolveRoot(f.Root) {
		_ = writeStderr("note: --root %q overrides AM_ROOT=%q\n", f.Root, envVal)
	}
}

// sessionName extracts the session name (last path component) from a resolved root path.
func sessionName(root string) string { return filepath.Base(root) }

// classifyRoot returns the base root for the given root, or "" if it cannot
// be determined. This is the single authoritative function for root classification.
//
// Resolution order:
//  1. AM_BASE_ROOT, when root is a direct session below it
//  2. If root's parent is the default root (.agent-mail), return that parent
//  3. The nearest root-aware .amqrc, when root is the configured base or a direct session below it
//  4. If root itself is the default root name (.agent-mail), return "" (known base)
//  5. If root is a session root (parent contains sibling session dirs), return parent
//  6. Otherwise, return "" (base or unknown — caller must handle)
func classifyRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	resolvedRoot := absPath(resolveRoot(root))
	if base := strings.TrimSpace(os.Getenv(envBaseRoot)); base != "" {
		base = absPath(resolveRoot(base))
		if resolvedRoot == base {
			return ""
		}
		if isSessionRootUnderBase(resolvedRoot, base) {
			return base
		}
	}
	parent := filepath.Dir(resolvedRoot)
	// The default layout convention is structural and deliberately outranks
	// root-local .amqrc, which must not rebase a session into its own base.
	if filepath.Base(parent) == defaultCoopRoot {
		return parent
	}
	if base := configuredBaseRoot(resolvedRoot); base != "" {
		if resolvedRoot == base {
			return ""
		}
		return base
	}
	if filepath.Base(resolvedRoot) == defaultCoopRoot {
		return ""
	}
	// Check if root looks like a session: parent has sibling dirs with agents/.
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == filepath.Base(resolvedRoot) {
			continue
		}
		if dirExists(filepath.Join(parent, e.Name(), "agents")) {
			return parent // Found a sibling session — root is a session, parent is base.
		}
	}
	return ""
}

// resolveSessionName returns the session name for the given root, or "" if
// the root is not inside a session. Uses classifyRoot to detect session context.
func resolveSessionName(root string) string {
	base := classifyRoot(root)
	if base == "" {
		return ""
	}
	// Ensure root actually differs from base (not just base root itself)
	if absPath(resolveRoot(root)) == absPath(resolveRoot(base)) {
		return ""
	}
	return sessionName(root)
}

// cachedAmqrcRoot returns the literal root from .amqrc, cached via sync.Once.
// Returns "" on any error (best-effort for defaulting, not validation).
var amqrcOnce sync.Once
var amqrcCachedRoot string

func cachedAmqrcRoot() string {
	amqrcOnce.Do(func() {
		result, err := findAndLoadAmqrc()
		if err != nil {
			return
		}
		root := result.Config.Root
		if root == "" {
			return
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(result.Dir, root)
		}
		amqrcCachedRoot = root
	})
	return amqrcCachedRoot
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
	return ".agent-mail"
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

func configuredBaseRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	result, err := findAmqrcForRoot(root)
	if err != nil {
		if !errors.Is(err, errAmqrcNotFound) {
			_ = writeStderr("warning: reading .amqrc for root %q: %v\n", root, err)
		}
		return ""
	}
	if result.Config.Root == "" {
		return ""
	}
	base := result.Config.Root
	if !filepath.IsAbs(base) {
		base = filepath.Join(result.Dir, base)
	}
	base = absPath(base)
	if isBaseOrSessionRoot(root, base) {
		return base
	}
	return ""
}

// isBaseOrSessionRoot returns true when root is either the base itself or a
// direct child session below it.
func isBaseOrSessionRoot(root, base string) bool {
	root = strings.TrimSpace(root)
	base = strings.TrimSpace(base)
	if root == "" || base == "" {
		return false
	}
	root = absPath(resolveRoot(root))
	base = absPath(resolveRoot(base))
	return root == base || filepath.Dir(root) == base
}

// isSessionRootUnderBase returns true when root is a direct child of base
// (i.e., root is a session directory like .agent-mail/collab under .agent-mail).
// Returns false when root == base (the base root itself is not a session).
func isSessionRootUnderBase(root, base string) bool {
	root = strings.TrimSpace(root)
	base = strings.TrimSpace(base)
	if root == "" || base == "" {
		return false
	}
	root = absPath(resolveRoot(root))
	base = absPath(resolveRoot(base))
	if root == base {
		return false
	}
	return filepath.Dir(root) == base
}

// baseRootOf returns the base root for a queue root: the derived/configured base
// when root is a session directory, or root itself otherwise.
func baseRootOf(root string) string {
	if base := classifyRoot(root); base != "" {
		return resolveRoot(base)
	}
	return resolveRoot(root)
}

// sameBaseTree reports whether two roots belong to the same base tree — the same
// project/base root, including any of its session subdirectories. This is the
// boundary used to decide whether an explicit --root crosses into a different
// AMQ tree than the caller's own.
func sameBaseTree(a, b string) bool {
	a, b = resolveRoot(a), resolveRoot(b)
	if a == "" || b == "" {
		return false
	}
	return baseRootOf(a) == baseRootOf(b)
}

// conflictingSourceRoot returns an established "home" root for the caller that
// belongs to a DIFFERENT base tree than target, when one is evident. Evidence is
// taken only from the caller's active session env — AM_ROOT and AM_BASE_ROOT,
// which coop exec / `amq env` set — never invented. It returns ok=false when
// there is no such evidence (bare-root scripts, CI, and tests passing --root to
// a temp dir), so those keep working untouched.
//
// This backs the cross-tree send guard (issue #144): a direct --root that
// crosses into another tree carries no sender-origin metadata, so the recipient
// cannot reply. Replyable cross-tree messaging must use --project/--session.
//
// Note: cwd-based project .amqrc is deliberately NOT consulted here. It would
// flag any `amq send --root X` run from inside a project directory, breaking
// hermetic scripts/harnesses for marginal coverage — the env signals already
// cover the coop-session case that motivated the guard. A direct --root send
// with no session env set folds into the documented residual.
func conflictingSourceRoot(target string) (string, bool) {
	target = resolveRoot(target)
	if target == "" {
		return "", false
	}
	for _, raw := range []string{
		strings.TrimSpace(os.Getenv(envRoot)),
		strings.TrimSpace(os.Getenv(envBaseRoot)),
	} {
		if raw == "" {
			continue
		}
		src := resolveRoot(raw)
		if src == "" || src == target {
			continue
		}
		if !sameBaseTree(src, target) {
			return src, true
		}
	}
	return "", false
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

	return withReservedHumanHandle(cfg.Agents), nil
}

func withReservedHumanHandle(agents []string) []string {
	if len(agents) == 0 {
		return agents
	}
	for _, agent := range agents {
		if agent == reservedHumanHandle {
			return agents
		}
	}
	out := make([]string, 0, len(agents)+1)
	out = append(out, agents...)
	out = append(out, reservedHumanHandle)
	return out
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

// readBody resolves a message body from the --body flag value.
//
// Sources:
//   - "", "-", "@-"   → read from stdin (standard CLI convention)
//   - "@<path>"       → read the named file
//   - anything else   → the literal string
//
// A send that loses its body should fail visibly, not deliver silently. When
// the resolved body is empty after trimming, readBody returns an error unless
// allowEmpty is set, so a dropped or mistyped body never ships as a blank
// message. The lone "-" footgun (previously delivered as a literal hyphen) is
// now treated as stdin and caught by the same empty check.
func readBody(bodyFlag string, allowEmpty bool) (string, error) {
	body, err := resolveBody(bodyFlag)
	if err != nil {
		return "", err
	}
	if !allowEmpty && strings.TrimSpace(body) == "" {
		return "", UsageError("empty body; pass --body \"text\", --body @file, pipe stdin, or --allow-empty to send a blank body")
	}
	return body, nil
}

func resolveBody(bodyFlag string) (string, error) {
	if bodyFlag == "" || bodyFlag == "-" || bodyFlag == "@-" {
		return readStdinBody()
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

// readStdinBody reads the body from stdin. If stdin is an interactive terminal
// there is nothing piped in, so it returns an empty string immediately rather
// than blocking on a read that would never see EOF; the caller's empty-body
// check then decides whether that is an error.
func readStdinBody() (string, error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return "", nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return string(data), nil
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
		return false, UsageError("%v", err)
	}
	for _, name := range []string{"root", "session"} {
		if fl := fs.Lookup(name); fl != nil && flagWasVisited(fs, name) && strings.TrimSpace(fl.Value.String()) == "" {
			return false, UsageError("--%s cannot be empty", name)
		}
	}
	return false, nil
}

func flagWasVisited(fs *flag.FlagSet, name string) bool {
	if fs == nil {
		return false
	}
	visited := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == name {
			visited = true
		}
	})
	return visited
}

// rejectPositionalArgs returns a UsageError if the flag set has any remaining
// positional arguments after parsing. Commands that don't accept positional
// args should call this immediately after parseFlags to prevent silent drops.
func rejectPositionalArgs(fs *flag.FlagSet, cmdName string) error {
	if remaining := fs.Args(); len(remaining) > 0 {
		return UsageError("%s does not accept positional arguments (got %q); use --body to pass message text", cmdName, strings.Join(remaining, " "))
	}
	return nil
}

func usageWithFlags(fs *flag.FlagSet, usage string, notes ...string) func() {
	return func() {
		writeUsageTo(os.Stdout, fs, usage, notes...)
	}
}

func usageWithHiddenFlags(fs *flag.FlagSet, usage string, hiddenFlags []string, notes ...string) func() {
	return func() {
		writeUsageToWithHiddenFlags(os.Stdout, fs, usage, hiddenFlags, notes...)
	}
}

// writeUsageTo renders usage text to the given writer.
func writeUsageTo(w io.Writer, fs *flag.FlagSet, usage string, notes ...string) {
	writeUsageToWithHiddenFlags(w, fs, usage, nil, notes...)
}

func writeUsageToWithHiddenFlags(w io.Writer, fs *flag.FlagSet, usage string, hiddenFlags []string, notes ...string) {
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintln(w, "  "+usage)
	if len(notes) > 0 {
		_, _ = fmt.Fprintln(w)
		for _, note := range notes {
			_, _ = fmt.Fprintln(w, note)
		}
	}
	_, _ = fmt.Fprintln(w)
	// Only print Options header if there are visible flags.
	flagDefaults := visibleFlagDefaults(fs, hiddenFlags)
	if flagDefaults != "" {
		_, _ = fmt.Fprintln(w, "Options:")
		_, _ = fmt.Fprint(w, flagDefaults)
	}
}

func visibleFlagDefaults(fs *flag.FlagSet, hiddenFlags []string) string {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	return filterHiddenFlagDefaults(buf.String(), hiddenFlags)
}

func filterHiddenFlagDefaults(defaults string, hiddenFlags []string) string {
	if defaults == "" || len(hiddenFlags) == 0 {
		return defaults
	}
	hidden := make(map[string]struct{}, len(hiddenFlags))
	for _, name := range hiddenFlags {
		hidden[name] = struct{}{}
	}
	var out strings.Builder
	skip := false
	for _, line := range strings.SplitAfter(defaults, "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		if name, ok := printedFlagName(trimmed); ok {
			_, skip = hidden[name]
		}
		if !skip {
			out.WriteString(line)
		}
	}
	return out.String()
}

func printedFlagName(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "--") {
		return "", false
	}
	name := strings.TrimPrefix(trimmed, "-")
	if name == "" {
		return "", false
	}
	if idx := strings.IndexAny(name, " \t"); idx >= 0 {
		name = name[:idx]
	}
	return name, true
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
