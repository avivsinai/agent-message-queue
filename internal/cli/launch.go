package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"golang.org/x/term"
)

type launchCLIOptions struct {
	session    string
	resumeOnly bool
}

var (
	launchIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	launchInput      = func() *bufio.Reader { return bufio.NewReader(os.Stdin) }
	launchStateDir   = defaultLaunchStateDir
	launchAMQPath    = func() string { return "amq" }
	launchAdapters   = defaultLaunchAdapters
	launchBackends   = func() map[string]launch.Backend {
		return map[string]launch.Backend{launch.LauncherCommands: launch.Commands{}}
	}
	launchHostname = os.Hostname
)

func runLaunch(args []string) error {
	return runLaunchEngine(args, launchCLIOptions{})
}

func runSessionResume(name string, args []string) error {
	return runLaunchEngine(args, launchCLIOptions{session: name, resumeOnly: true})
}

func runLaunchEngine(args []string, options launchCLIOptions) error {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	common := addCommonFlags(fs)
	sessionFlag := fs.String("session", options.session, "Named session to launch or resume")
	launcherFlag := fs.String("launcher", launch.LauncherAuto, "Launcher backend (auto or commands)")
	freshFlag := fs.Bool("fresh", false, "Start fresh conversations for this launch")
	allowFreshFlag := fs.Bool("allow-fresh-fallback", false, "Allow fresh conversations when saved identities are stale")
	rebindFlag := fs.Bool("rebind", false, "Confirm a deliberate launcher binding change")
	usageName := "amq launch [options]"
	if options.resumeOnly {
		usageName = "amq session resume <name> [options]"
	}
	usage := usageWithFlags(fs, usageName,
		"Reconcile one existing AMQ session from the committed launch declaration.",
		"Unknown sessions are never created. Non-interactive trust, stale identity, and Inspect uncertainty exit 6.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *freshFlag && *allowFreshFlag {
		return UsageError("--fresh and --allow-fresh-fallback are mutually exclusive")
	}
	if flagWasVisited(fs, "session") && strings.TrimSpace(*sessionFlag) == "" {
		return UsageError("--session must not be blank")
	}
	if options.resumeOnly && flagWasVisited(fs, "session") {
		return UsageError("session resume takes its session name as a positional argument")
	}
	if strings.TrimSpace(*launcherFlag) == "" {
		return UsageError("--launcher must not be blank")
	}
	if *rebindFlag && strings.TrimSpace(*launcherFlag) == launch.LauncherAuto {
		return UsageError("--rebind requires an explicit --launcher")
	}

	cfg, present, err := loadProjectLaunchConfig()
	if err != nil {
		return err
	}
	if !present {
		return NotFoundError("committed launch config not found; run 'amq setup'")
	}
	projectConfigPath, _, err := findProjectLaunchJSONPath()
	if err != nil {
		return err
	}
	projectRoot := filepath.Dir(filepath.Dir(projectConfigPath))
	session := strings.TrimSpace(*sessionFlag)
	if session == "" && !common.rootExplicit() {
		session = cfg.DefaultSession
	}
	if common.rootExplicit() {
		if options.resumeOnly {
			return UsageError("session resume does not accept --root; use amq launch --root <session-root>")
		}
		if flagWasVisited(fs, "session") {
			return UsageError("--session and --root are mutually exclusive")
		}
		session = resolveSessionName(common.Root)
		if session == "" {
			return UsageError("--root must select a named session root")
		}
	}
	if err := validateSessionName(session); err != nil {
		return UsageError("--session: %v", err)
	}
	routeSession := session
	if common.rootExplicit() {
		routeSession = ""
	}
	root, routed, err := resolveMailboxRoot(common, routeSession)
	if err != nil {
		return err
	}
	if err := guardMailboxContext("launch", root, routed, false, common.rootExplicit()); err != nil {
		return err
	}
	for _, agent := range cfg.Agents {
		if err := requireMailbox(root, agent.Handle); err != nil {
			return err
		}
	}
	identity, err := snapshotMailboxDeliveryRoot(root, routed, false)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()

	local := launch.LocalConfig{Schema: launch.LocalConfigSchema, LauncherPreference: []string{launch.LauncherCommands}}
	localPath := filepath.Join(projectRoot, setupLocalConfigPath)
	if parsed, _, exists, loadErr := loadOptionalLocalConfig(localPath); loadErr != nil {
		return loadErr
	} else if exists {
		local = parsed
	}
	stateDir, err := launchStateDir()
	if err != nil {
		return err
	}
	trust, err := launch.OpenTrustStore(stateDir, projectRoot)
	if err != nil {
		return ActionRequiredError("open launch trust store: %v", err)
	}
	adapters := launchAdapters(cfg)
	host, _ := launchHostname()
	request := launch.ReconcileRequest{
		ProjectRoot: projectRoot, Session: session, AMQPath: launchAMQPath(), Root: deliveryRoot,
		Config: cfg, Launcher: strings.TrimSpace(*launcherFlag), Preferences: local.LauncherPreference,
		Backends: launchBackends(), Adapters: adapters,
		TrustStore: trust, Fresh: *freshFlag, AllowFreshFallback: *allowFreshFlag,
		ResumeOnly: options.resumeOnly, Rebind: *rebindFlag, HostIdentity: host,
	}
	interactive := !common.JSON && launchIsTerminal()
	if interactive {
		reader := launchInput()
		request.ConfirmTrust = func(plan launch.Plan, digest string) (bool, error) {
			data, err := json.MarshalIndent(plan, "", "  ")
			if err != nil {
				return false, err
			}
			if err := writeStdout("Launch plan (%s):\n%s\n", digest, data); err != nil {
				return false, err
			}
			return setupConfirm(reader, "Trust and execute this plan?")
		}
		request.ConfirmRebind = func(binding launch.BindingRecord, foreign bool) (launch.RebindDisposition, bool, error) {
			defaultDisposition := string(launch.RebindClose)
			if foreign {
				defaultDisposition = string(launch.RebindLeave)
			}
			value, err := setupPromptLine(reader, "Old resource disposition (close or leave)", defaultDisposition)
			if err != nil {
				return "", false, err
			}
			disposition := launch.RebindDisposition(value)
			confirmed, err := setupConfirm(reader, fmt.Sprintf("Rebind from %s with disposition %s?", binding.Backend, disposition))
			return disposition, confirmed, err
		}
	}
	result, err := launch.Reconcile(request)
	if err != nil {
		return err
	}
	if err := outputLaunchResult(common.JSON, result); err != nil {
		return err
	}
	if result.AggregateCode == ExitSuccess {
		return nil
	}
	reason := result.Reason
	if reason == "" {
		reason = "launch did not complete"
	}
	switch result.AggregateCode {
	case ExitActionRequired:
		return ActionRequiredError("%s", reason)
	case ExitTimeout:
		return TimeoutError("%s", reason)
	default:
		return WithExitCode(ExitError, errors.New(reason))
	}
}

func defaultLaunchAdapters(cfg launch.ProjectConfig) map[string]launch.HarnessAdapter {
	adapters := make(map[string]launch.HarnessAdapter, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		switch agent.Adapter {
		case launch.ClaudeProvider:
			adapters[agent.Adapter] = launch.NewClaudeAdapter(agent.Command[0])
		case launch.CodexProvider:
			adapters[agent.Adapter] = launch.NewCodexAdapter(agent.Command[0])
		}
	}
	return adapters
}

func outputLaunchResult(jsonOutput bool, result launch.ReconcileResult) error {
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}
	if result.Backend != "" {
		if err := writeStdout("Session %s: %s via %s\n", result.Session, result.Outcome, result.Backend); err != nil {
			return err
		}
	}
	for _, agent := range result.Agents {
		if err := writeStdout("  %s: %s", agent.Handle, agent.ConversationDisposition); err != nil {
			return err
		}
		if agent.Reason != "" {
			if err := writeStdout(" (%s)", agent.Reason); err != nil {
				return err
			}
		}
		if err := writeStdout("\n"); err != nil {
			return err
		}
	}
	for _, command := range result.Commands {
		if err := writeStdout("  %s\n", command.Line); err != nil {
			return err
		}
	}
	return nil
}

func defaultLaunchStateDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); value != "" {
		return filepath.Join(value, "amq"), nil
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "amq", "state"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "amq"), nil
}
