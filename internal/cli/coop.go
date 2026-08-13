package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	defaultCoopRoot    = ".agent-mail"
	defaultCoopAgents  = "claude,codex,user"
	defaultSessionName = "collab"
)

func runCoop(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printGroupUsage(findCommand("coop"))
	}

	switch args[0] {
	case "init":
		return runCoopInit(args[1:])
	case "exec":
		return runCoopExec(args[1:])
	default:
		return formatUnknownSubcommand("coop", args[0])
	}
}

func runCoopInit(args []string) error {
	return runCoopInitInternal(args, true)
}

func runCoopInitInternal(args []string, printNextSteps bool) (returnErr error) {
	fs := flag.NewFlagSet("coop init", flag.ContinueOnError)
	rootFlag := fs.String("root", defaultCoopRoot, "Root directory for the queue")
	agentsFlag := fs.String("agents", defaultCoopAgents, "Comma-separated agent handles")
	forceFlag := fs.Bool("force", false, "Overwrite existing config if present")
	jsonFlag := fs.Bool("json", false, "Output as JSON")
	noGitignoreFlag := fs.Bool("no-gitignore", false, "Do not modify .gitignore")

	usage := usageWithFlags(fs, "amq coop init [options]",
		"Initialize a project for co-op mode with sensible defaults.",
		"",
		"Creates:",
		"  - .amqrc file with root configuration",
		"  - Mailbox directories for each agent",
		"  - Updates .gitignore (unless --no-gitignore)",
		"",
		"Defaults:",
		fmt.Sprintf("  --root=%s  --agents=%s", defaultCoopRoot, defaultCoopAgents),
		"",
		"Explicit three-engine example (not a default):",
		"  --agents claude,codex,grok,user",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	rootExplicit := flagWasVisited(fs, "root")
	if !rootExplicit {
		if _, existingErr := findAndLoadAmqrc(); errors.Is(existingErr, errAmqrcNotFound) {
			gitBoundary, insideGit := gitWorktreeRootFromCWD()
			if top, worktree := gitWorktreeTopFromCWD(); worktree {
				cwd, cwdErr := os.Getwd()
				if cwdErr != nil {
					return cwdErr
				}
				if !sameTreeIdentity(cwd, top) {
					if err := os.Chdir(top); err != nil {
						return fmt.Errorf("enter Git worktree top %q: %w", top, err)
					}
					resetAmqrcCache()
					defer func() {
						if err := os.Chdir(cwd); err != nil {
							returnErr = errors.Join(returnErr, fmt.Errorf("restore working directory %q: %w", cwd, err))
						}
						resetAmqrcCache()
					}()
				}
			} else if insideGit {
				return noEligibleRootInGitError(gitBoundary)
			}
		}
	}

	explicitAgents := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "agents" {
			explicitAgents = true
		}
	})

	root := *rootFlag

	// Check if already initialized (search parents too)
	existing, existingErr := findAndLoadAmqrc()
	amqrcPath := ".amqrc"
	amqrcExistsInCwd := false
	if _, err := os.Stat(amqrcPath); err == nil {
		amqrcExistsInCwd = true
	}

	// Handle .amqrc detection results
	if existingErr == nil {
		cwd, _ := os.Getwd()
		if existing.Dir != cwd {
			// Found in parent directory - warn about subdirectory init
			if !*forceFlag {
				return fmt.Errorf("already initialized in parent directory %s (root=%s). Use --force to create a separate .amqrc here", existing.Dir, existing.Config.Root)
			}
		} else if existing.Config.Root != root && !*forceFlag {
			// Same directory but different root
			return fmt.Errorf(".amqrc already exists with root=%q (use --force to overwrite)", existing.Config.Root)
		}
		// Same directory, same root (or --force): continue to ensure dirs exist
	} else if existingErr != errAmqrcNotFound {
		// Parse error or read error in .amqrc - surface it unless --force
		if !*forceFlag {
			return fmt.Errorf("invalid .amqrc found: %w (use --force to overwrite)", existingErr)
		}
		// With --force, warn but continue
		_ = writeStderr("warning: %v (overwriting with --force)\n", existingErr)
	}

	// The root in .amqrc is the literal queue root.
	queueRoot := root

	cfgPath := filepath.Join(queueRoot, "meta", "config.json")
	var agents []string
	writeConfig := *forceFlag
	if !*forceFlag {
		_, lstatErr := os.Lstat(cfgPath)
		switch {
		case lstatErr == nil:
			cfg, loadErr := config.LoadConfig(cfgPath)
			if loadErr != nil {
				return fmt.Errorf("failed to load existing config: %w", loadErr)
			}
			agents = append([]string(nil), cfg.Agents...)
			if len(agents) == 0 {
				return fmt.Errorf("existing config has no agents")
			}
			for _, agent := range agents {
				if err := fsq.ValidateHandle(agent); err != nil {
					return fmt.Errorf("invalid agent in existing config: %w", err)
				}
			}
			if explicitAgents {
				requestedAgents, err := parseCoopInitAgents(*agentsFlag)
				if err != nil {
					return err
				}
				configuredRoster := dedupeStrings(append([]string(nil), agents...))
				sort.Strings(configuredRoster)
				if !slices.Equal(requestedAgents, configuredRoster) {
					_ = writeStderr(
						"warning: using existing config agents %s; use --force to overwrite\n",
						strings.Join(agents, ","),
					)
				}
			}
		case os.IsNotExist(lstatErr):
			writeConfig = true
		default:
			return fmt.Errorf("failed to inspect existing config: %w", lstatErr)
		}
	}
	if writeConfig {
		parsedAgents, err := parseCoopInitAgents(*agentsFlag)
		if err != nil {
			return err
		}
		agents = parsedAgents
	}

	// Keep the shared config at the base root, but provision the roster in the
	// default session that coop exec actually selects.
	if err := provisionCoopBaseAndSession(queueRoot, defaultSessionName, agents); err != nil {
		return err
	}

	configWritten := false
	if writeConfig {
		cfg := config.Config{
			Version:    format.CurrentVersion,
			CreatedUTC: time.Now().UTC().Format(time.RFC3339),
			Agents:     agents,
		}
		if err := config.WriteConfig(cfgPath, cfg, *forceFlag); err != nil {
			return fmt.Errorf("failed to write config: %w", err)
		}
		configWritten = true
	}

	// Write .amqrc only if it doesn't exist in cwd or --force is set
	amqrcWritten := false
	if !amqrcExistsInCwd || *forceFlag {
		amqrcData := amqrc{Root: root}
		amqrcJSON, err := json.MarshalIndent(amqrcData, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal .amqrc: %w", err)
		}
		if err := os.WriteFile(amqrcPath, append(amqrcJSON, '\n'), 0644); err != nil {
			return fmt.Errorf("failed to write .amqrc: %w", err)
		}
		amqrcWritten = true
	}

	// Update .gitignore (creates if needed, only for relative paths) unless opted out
	gitignoreUpdated := false
	if !*noGitignoreFlag {
		gitignoreUpdated = ensureGitignore(root)
	}

	// Output
	if *jsonFlag {
		out := struct {
			Root             string   `json:"root"`
			Agents           []string `json:"agents"`
			AmqrcWritten     bool     `json:"amqrc_written"`
			ConfigWritten    bool     `json:"config_written"`
			GitignoreUpdated bool     `json:"gitignore_updated"`
		}{
			Root:             root,
			Agents:           agents,
			AmqrcWritten:     amqrcWritten,
			ConfigWritten:    configWritten,
			GitignoreUpdated: gitignoreUpdated,
		}
		return writeJSON(os.Stdout, out)
	}

	if err := writeStdout("Initialized co-op mode:\n"); err != nil {
		return err
	}
	if err := writeStdout("  Root: %s\n", root); err != nil {
		return err
	}
	if err := writeStdout("  Agents: %s\n", strings.Join(agents, ", ")); err != nil {
		return err
	}
	if amqrcWritten {
		if err := writeStdout("  Created: .amqrc\n"); err != nil {
			return err
		}
	}
	if gitignoreUpdated {
		if err := writeStdout("  Updated: .gitignore\n"); err != nil {
			return err
		}
	}
	if printNextSteps {
		if err := writeStdoutLine(""); err != nil {
			return err
		}
		if err := writeStdoutLine("Next steps:"); err != nil {
			return err
		}
		terminalNum := 1
		for _, agent := range agents {
			if agent == reservedHumanHandle {
				continue
			}
			if err := writeStdout("  Terminal %d: amq coop exec %s\n", terminalNum, agent); err != nil {
				return err
			}
			terminalNum++
		}
		if err := writeStdoutLine("  (handle = command basename; custom handle: amq coop exec --me <handle> <command>)"); err != nil {
			return err
		}
		if err := writeStdoutLine(""); err != nil {
			return err
		}
		if err := writeStdoutLine("Tip: eval \"$(amq shell-setup)\" to add co-op aliases to your shell"); err != nil {
			return err
		}
	}
	return nil
}

// provisionCoopSession creates or validates one named session as a direct,
// non-symlink child of a pinned base capability, then creates every requested
// mailbox through the pinned child capability. No provisioning write reopens
// the session through its ambient lexical path.
func provisionCoopSession(base, session string, agents []string, execAgent, execCommand string) (string, error) {
	return provisionCoopSessionChild(base, session, agents, execAgent, execCommand, false)
}

// provisionCoopBaseAndSession is the shared provisioning primitive used by
// coop init plumbing and setup porcelain. It retains the established base
// compatibility mailboxes and the pinned named-session creation path.
func provisionCoopBaseAndSession(base, session string, agents []string) error {
	if err := fsq.EnsureRootDirs(base); err != nil {
		return fmt.Errorf("failed to create root directories: %w", err)
	}
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(base, agent); err != nil {
			return fmt.Errorf("failed to create compatibility base mailbox for %s: %w", agent, err)
		}
	}
	if _, err := provisionCoopSession(base, session, agents, "", ""); err != nil {
		return fmt.Errorf("failed to create default session root: %w", err)
	}
	return nil
}

// provisionNewNamedSession is session create: exclusive child creation so a
// racing creator cannot silently open an existing session.
func provisionNewNamedSession(base, session string, agents []string) (string, error) {
	return provisionCoopSessionChild(base, session, agents, "", "", true)
}

// sessionCreateBeforeExclusive runs after the base is pinned and before the
// exclusive mkdir so tests can inject a racing creator.
var sessionCreateBeforeExclusive func(base, name string)

func provisionCoopSessionChild(base, session string, agents []string, execAgent, execCommand string, exclusive bool) (string, error) {
	if err := validateSessionName(session); err != nil {
		return "", err
	}
	base, err := absoluteSessionRoot(base)
	if err != nil {
		return "", err
	}
	if !dirExists(base) {
		return "", NotFoundError("base root not found at %s", base)
	}
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		return "", err
	}
	baseRoot, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		return "", err
	}
	defer func() { _ = baseRoot.Close() }()

	if exclusive && sessionCreateBeforeExclusive != nil {
		sessionCreateBeforeExclusive(base, session)
	}
	var sessionRoot *fsq.DeliveryRoot
	if exclusive {
		sessionRoot, err = baseRoot.CreateDirectChildExclusive(session, 0o700)
	} else {
		sessionRoot, err = baseRoot.OpenOrCreateDirectChild(session, 0o700)
	}
	if err != nil {
		if exclusive {
			var exists *fsq.DirectChildExistsError
			if errors.As(err, &exists) {
				return "", err
			}
		}
		sessionPath := filepath.Join(base, session)
		if info, lstatErr := os.Lstat(sessionPath); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			message := fmt.Sprintf(
				"session root %s is a symlink; refusing to provision through it; remove it to use the named session",
				sessionPath,
			)
			if target, resolveErr := filepath.EvalSymlinks(sessionPath); resolveErr == nil &&
				execAgent != "" && execCommand != "" {
				message += fmt.Sprintf(
					"; for intentional relocation, use: amq coop exec --root %s --me %s %s",
					shellQuoteArg(target),
					shellQuoteArg(execAgent),
					shellQuoteArg(execCommand),
				)
			}
			return "", ContextMismatchError("%s", message)
		}
		return "", ContextMismatchError(
			"refusing session %q: path %s is not a stable direct directory under base: %v",
			session,
			sessionPath,
			err,
		)
	}
	defer func() { _ = sessionRoot.Close() }()
	if err := sessionRoot.EnsureRootDirs(); err != nil {
		return "", err
	}
	for _, agent := range agents {
		if err := sessionRoot.EnsureAgentDirs(agent); err != nil {
			return "", fmt.Errorf("failed to create mailbox for %s: %w", agent, err)
		}
	}
	return sessionRoot.Base(), nil
}

func parseCoopInitAgents(raw string) ([]string, error) {
	agents, err := parseHandles(raw)
	if err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return nil, UsageError("at least one agent required")
	}
	agents = dedupeStrings(agents)
	sort.Strings(agents)
	return agents, nil
}
