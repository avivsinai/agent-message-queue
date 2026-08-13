package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runSession(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printGroupUsage(findCommand("session"))
	}
	switch args[0] {
	case "create":
		return runSessionCreate(args[1:])
	case "list":
		return runSessionList(args[1:])
	default:
		return formatUnknownSubcommand("session", args[0])
	}
}

type sessionExistsError struct {
	Name string
	Path string
}

func (e *sessionExistsError) Error() string {
	return fmt.Sprintf("session %q already exists at %s", e.Name, e.Path)
}

type sessionCreateResult struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Agents []string `json:"agents"`
}

func runSessionCreate(args []string) error {
	fs := flag.NewFlagSet("session create", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(fs, "amq session create <name> [options]",
		"Create a named session and its roster mailboxes under the base root.",
		"Canonical names only ([a-z0-9_-]+). Existing sessions fail loudly; create is never silent.")
	name, flagArgs, peelErr := peelSessionCreateName(fs, args)
	if peelErr != nil {
		flagArgs = args
	}
	if handled, err := parseFlags(fs, flagArgs, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if peelErr != nil {
		return peelErr
	}
	if err := validateSessionName(name); err != nil {
		return err
	}

	base, err := sessionCreateBaseRoot(common.Root)
	if err != nil {
		return err
	}

	agents, err := loadSessionCreateAgents(base)
	if err != nil {
		return err
	}
	created, err := provisionNewNamedSession(base, name, agents)
	if err != nil {
		var exists *fsq.DirectChildExistsError
		if errors.As(err, &exists) {
			return &sessionExistsError{Name: name, Path: filepath.Join(base, name)}
		}
		return err
	}
	if agents == nil {
		agents = []string{}
	}
	result := sessionCreateResult{Name: name, Path: created, Agents: agents}
	if common.JSON {
		return writeJSON(os.Stdout, result)
	}
	return writeStdout("Created session %q at %s\n", name, created)
}

type sessionListEntry struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Kind   string   `json:"kind"`
	Agents []string `json:"agents"`
	Hint   string   `json:"hint"`
}

type sessionSkipEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type sessionListResult struct {
	Sessions []sessionListEntry `json:"sessions"`
	Skipped  []sessionSkipEntry `json:"skipped,omitempty"`
}

func runSessionList(args []string) error {
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(fs, "amq session list [options]",
		"List named sessions under the base root.",
		"Canonical sessions are listed normally. Safe legacy roots are marked legacy_name with an exact --root hint.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	base, err := sessionBaseRoot(common.Root)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}

	result := sessionListResult{Sessions: []sessionListEntry{}}
	for _, entry := range entries {
		item, skipped, skip := classifySessionChild(base, entry.Name())
		if skip {
			result.Skipped = append(result.Skipped, skipped)
			continue
		}
		result.Sessions = append(result.Sessions, item)
	}
	sort.Slice(result.Sessions, func(i, j int) bool {
		return result.Sessions[i].Name < result.Sessions[j].Name
	})
	sort.Slice(result.Skipped, func(i, j int) bool {
		return result.Skipped[i].Name < result.Skipped[j].Name
	})

	if common.JSON {
		return writeJSON(os.Stdout, result)
	}
	if len(result.Sessions) == 0 {
		return writeStdoutLine("No sessions found.")
	}
	width := 0
	for _, item := range result.Sessions {
		if len(item.Name) > width {
			width = len(item.Name)
		}
	}
	for _, item := range result.Sessions {
		agents := strings.Join(item.Agents, ", ")
		if item.Kind == sessionKindLegacy {
			if err := writeStdout("  %-*s  (legacy_name)  use: %s\n", width, item.Name, item.Hint); err != nil {
				return err
			}
			continue
		}
		if err := writeStdout("  %-*s  %s\n", width, item.Name, agents); err != nil {
			return err
		}
	}
	return nil
}

func sessionBaseRoot(root string) (string, error) {
	base := absPath(baseRootOf(resolveRoot(root)))
	if !dirExists(base) {
		return "", NotFoundError("base root not found at %s", base)
	}
	return base, nil
}

func sessionCreateBaseRoot(root string) (string, error) {
	base := absPath(baseRootOf(resolveRoot(root)))
	if dirExists(base) {
		return base, nil
	}
	_, err := findAndLoadAmqrc()
	if errors.Is(err, errAmqrcNotFound) {
		return "", NotFoundError("base root not found at %s; run 'amq setup' or 'amq coop init'", base)
	}
	if err != nil {
		return "", err
	}
	if err := fsq.EnsureRootDirs(base); err != nil {
		return "", fmt.Errorf("failed to create base root %q: %w", base, err)
	}
	return base, nil
}

func loadSessionCreateAgents(base string) ([]string, error) {
	if handles, ok, err := loadLaunchRosterHandles(); err != nil {
		return nil, err
	} else if ok {
		return handles, nil
	}
	cfg, err := config.LoadConfig(filepath.Join(base, "meta", "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return cfg.Agents, nil
}

func loadLaunchRosterHandles() ([]string, bool, error) {
	// WA5-superseded peek: the authoritative launch.json parser lands with setup.
	result, err := findAndLoadAmqrc()
	if errors.Is(err, errAmqrcNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	path := filepath.Join(result.Dir, ".amq", "launch.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var parsed struct {
		Agents []struct {
			Handle string `json:"handle"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, false, fmt.Errorf("parse .amq/launch.json: %w", err)
	}
	handles := make([]string, 0, len(parsed.Agents))
	seen := map[string]bool{}
	for _, agent := range parsed.Agents {
		handle := strings.TrimSpace(agent.Handle)
		if handle == "" {
			return nil, false, fmt.Errorf("launch.json roster handle is empty")
		}
		normalized, err := normalizeHandle(handle)
		if err != nil {
			return nil, false, fmt.Errorf("launch.json roster handle %q: %w", handle, err)
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		handles = append(handles, normalized)
	}
	return handles, true, nil
}

func peelSessionCreateName(fs *flag.FlagSet, args []string) (string, []string, error) {
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			if i+1 >= len(args) {
				return "", nil, UsageError("session name required (e.g., amq session create feature-x)")
			}
			name := args[i+1]
			rest := append(append([]string{}, args[:i]...), args[i+2:]...)
			return name, rest, nil
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			span := sessionCreateFlagSpan(fs, arg)
			if i+span > len(args) {
				break
			}
			i += span
			continue
		}
		rest := append(append([]string{}, args[:i]...), args[i+1:]...)
		return arg, rest, nil
	}
	return "", nil, UsageError("session name required (e.g., amq session create feature-x)")
}

func sessionCreateFlagSpan(fs *flag.FlagSet, arg string) int {
	name := strings.TrimLeft(arg, "-")
	if strings.Contains(name, "=") {
		return 1
	}
	fl := fs.Lookup(name)
	if fl == nil {
		return 1
	}
	type boolFlag interface {
		IsBoolFlag() bool
	}
	if bf, ok := fl.Value.(boolFlag); ok && bf.IsBoolFlag() {
		return 1
	}
	return 2
}

func classifySessionChild(base, name string) (sessionListEntry, sessionSkipEntry, bool) {
	path := filepath.Join(base, name)
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	skip := func(reason string) (sessionListEntry, sessionSkipEntry, bool) {
		return sessionListEntry{}, sessionSkipEntry{Name: name, Path: abs, Reason: reason}, true
	}
	info, err := os.Lstat(path)
	if err != nil {
		return skip("unreadable")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return skip("symlink")
	}
	if !info.IsDir() {
		return skip("not_a_directory")
	}
	agentsDir := filepath.Join(path, "agents")
	agentsInfo, err := os.Lstat(agentsDir)
	if err != nil || !agentsInfo.IsDir() || agentsInfo.Mode()&os.ModeSymlink != 0 {
		return skip("not_a_session")
	}

	agents := listSessionAgents(path)
	switch {
	case isCanonicalSessionName(name):
		return sessionListEntry{
			Name:   name,
			Path:   abs,
			Kind:   sessionKindCanonical,
			Agents: agents,
		}, sessionSkipEntry{}, false
	case isSafeLegacySessionName(name):
		return sessionListEntry{
			Name:   name,
			Path:   abs,
			Kind:   sessionKindLegacy,
			Agents: agents,
			Hint:   fmt.Sprintf("amq list --root %s", abs),
		}, sessionSkipEntry{}, false
	default:
		return skip("non_canonical_name")
	}
}

func listSessionAgents(sessionRoot string) []string {
	entries, err := os.ReadDir(filepath.Join(sessionRoot, "agents"))
	if err != nil {
		return []string{}
	}
	var agents []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agents = append(agents, entry.Name())
	}
	sort.Strings(agents)
	if agents == nil {
		return []string{}
	}
	return agents
}
