package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"golang.org/x/term"
)

const (
	setupConfigPath      = ".amq/launch.json"
	setupLocalConfigPath = ".amq/launch.local.json"
)

type setupChange struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

type setupPreview struct {
	Digest             string                      `json:"digest"`
	ProjectRoot        string                      `json:"project_root"`
	QueueRoot          string                      `json:"queue_root"`
	DefaultSession     string                      `json:"default_session"`
	Agents             []launch.ProjectAgentConfig `json:"agents"`
	Layout             launch.LayoutIntent         `json:"layout"`
	LauncherPreference []string                    `json:"launcher_preference"`
	AvailableLaunchers []string                    `json:"available_launchers"`
	Changes            []setupChange               `json:"changes"`
}

type setupResult struct {
	Status  string       `json:"status"`
	Preview setupPreview `json:"preview"`
	Written []string     `json:"written"`
}

type setupConfigRefusalError struct {
	Handle  string
	Adapter string
	Err     error
}

func (e *setupConfigRefusalError) Error() string {
	return fmt.Sprintf("refused committed configuration for agent %q through adapter %q: %v", e.Handle, e.Adapter, e.Err)
}

func (e *setupConfigRefusalError) Unwrap() error { return e.Err }

type setupState struct {
	projectRoot     string
	queueRoot       string
	projectConfig   launch.ProjectConfig
	localConfig     launch.LocalConfig
	projectData     []byte
	localData       []byte
	amqrcData       []byte
	gitignoreData   []byte
	baseConfigData  []byte
	unionConfigData []byte
	needsProvision  bool
	changes         []setupChange
	available       []string
	noGitignore     bool
}

var (
	setupHarnessAdapters = func() []launch.HarnessAdapter {
		return []launch.HarnessAdapter{
			launch.NewClaudeAdapter(launch.ClaudeProvider),
			launch.NewCodexAdapter(launch.CodexProvider),
			launch.NewCursorAdapter(launch.CursorProvider),
		}
	}
	setupLookPath      = exec.LookPath
	setupCmuxAvailable = func() bool {
		return launch.NewCmuxBackend("").Detect().Available
	}
	setupIsTerminal = func() bool {
		return term.IsTerminal(int(os.Stdin.Fd()))
	}
	// setupCommitStepHook is a fault-injection seam. Production leaves it nil.
	setupCommitStepHook func(string) error
)

func runSetup(args []string) (returnErr error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	rootFlag := fs.String("root", defaultCoopRoot, "Queue root written to .amqrc")
	agentsFlag := fs.String("agents", "", "Comma-separated detected agent adapters")
	sessionFlag := fs.String("default-session", "", "Default session name")
	launchersFlag := fs.String("launcher-preference", "", "Comma-separated launcher preference")
	layoutFlag := fs.String("layout", launch.LayoutColumns, "Advisory layout intent")
	noGitignoreFlag := fs.Bool("no-gitignore", false, "Do not modify .gitignore")
	previewFlag := fs.Bool("preview", false, "Preview changes without writing")
	applyFlag := fs.String("apply", "", "Apply only when the recomputed preview matches this digest")
	yesFlag := fs.Bool("y", false, "Accept the preview without prompting")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")
	usage := usageWithFlags(fs, "amq setup [options]",
		"Configure this project after previewing every change.",
		"Detects Claude and Codex through their adapter capability probes.",
		"Launcher detection is probe-only; setup never calls a launcher backend.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	applyExplicit := flagWasVisited(fs, "apply")
	if *previewFlag && *yesFlag {
		return UsageError("--preview and -y are mutually exclusive")
	}
	if *previewFlag && applyExplicit {
		return UsageError("--preview and --apply are mutually exclusive")
	}
	if applyExplicit && *yesFlag {
		return UsageError("--apply and -y are mutually exclusive")
	}
	if applyExplicit && !validSetupDigest(*applyFlag) {
		return UsageError("--apply requires a sha256:<hex> digest")
	}
	nonInteractive := *previewFlag || applyExplicit || *yesFlag
	if *jsonFlag && !nonInteractive {
		return UsageError("--json requires --preview, --apply, or -y")
	}
	if !nonInteractive && !setupIsTerminal() {
		return UsageError("non-interactive setup requires -y")
	}
	if *layoutFlag != launch.LayoutColumns {
		return UsageError("unsupported --layout %q", *layoutFlag)
	}

	originalCWD, err := os.Getwd()
	if err != nil {
		return err
	}
	projectRoot := originalCWD
	if top, worktree := gitWorktreeTopFromCWD(); worktree {
		projectRoot = top
	} else if boundary, insideGit := gitWorktreeRootFromCWD(); insideGit {
		return noEligibleRootInGitError(boundary)
	}
	if !sameTreeIdentity(originalCWD, projectRoot) {
		if err := os.Chdir(projectRoot); err != nil {
			return fmt.Errorf("enter Git worktree top %q: %w", projectRoot, err)
		}
		resetAmqrcCache()
		defer func() {
			if err := os.Chdir(originalCWD); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore working directory %q: %w", originalCWD, err))
			}
			resetAmqrcCache()
		}()
	}

	var input *bufio.Reader
	if !nonInteractive {
		input = bufio.NewReader(os.Stdin)
	}
	state, err := buildSetupState(setupOptions{
		projectRoot: projectRoot,
		root:        *rootFlag, rootExplicit: flagWasVisited(fs, "root"),
		agents: *agentsFlag, agentsExplicit: flagWasVisited(fs, "agents"),
		defaultSession: *sessionFlag, sessionExplicit: flagWasVisited(fs, "default-session"),
		launchers: *launchersFlag, launchersExplicit: flagWasVisited(fs, "launcher-preference"),
		layout: *layoutFlag, noGitignore: *noGitignoreFlag, nonInteractive: nonInteractive, input: input,
	})
	if err != nil {
		return err
	}
	preview, err := state.preview()
	if err != nil {
		return err
	}
	if !*jsonFlag {
		if err := printSetupPreview(preview); err != nil {
			return err
		}
	}
	if *previewFlag {
		if *jsonFlag {
			return writeJSON(os.Stdout, setupResult{Status: "preview", Preview: preview, Written: []string{}})
		}
		return nil
	}
	if applyExplicit && *applyFlag != preview.Digest {
		return ActionRequiredError("setup approval digest mismatch: approved %s, recomputed %s", *applyFlag, preview.Digest)
	}
	if !nonInteractive {
		confirmed, err := setupConfirm(input, "Create this setup?")
		if err != nil {
			return err
		}
		if !confirmed {
			return writeStdoutLine("Aborted.")
		}
	}
	written, err := commitSetup(state)
	if err != nil {
		return err
	}
	status := "configured"
	if len(written) == 0 {
		status = "unchanged"
	}
	result := setupResult{Status: status, Preview: preview, Written: written}
	if *jsonFlag {
		return writeJSON(os.Stdout, result)
	}
	if status == "unchanged" {
		return writeStdoutLine("Setup already matches; no writes performed.")
	}
	return writeStdout("Setup committed (%d step(s)).\n", len(written))
}

type setupOptions struct {
	projectRoot                        string
	root, agents, defaultSession       string
	launchers, layout                  string
	rootExplicit, agentsExplicit       bool
	sessionExplicit, launchersExplicit bool
	noGitignore, nonInteractive        bool
	input                              *bufio.Reader
}

func buildSetupState(options setupOptions) (setupState, error) {
	if err := validateSetupConfigDirectory(); err != nil {
		return setupState{}, err
	}
	existingProject, existingProjectData, projectExists, err := loadOptionalProjectConfig(setupConfigPath)
	if err != nil {
		return setupState{}, err
	}
	existingLocal, existingLocalData, localExists, err := loadOptionalLocalConfig(setupLocalConfigPath)
	if err != nil {
		return setupState{}, err
	}
	if !projectExists && options.nonInteractive &&
		(!options.agentsExplicit || !options.sessionExplicit || !options.launchersExplicit) {
		return setupState{}, UsageError("first non-interactive setup requires explicit --agents, --default-session, and --launcher-preference")
	}

	root, amqrcData, amqrcExists, err := setupRoot(options.root, options.rootExplicit, options.projectRoot)
	if err != nil {
		return setupState{}, err
	}
	adapters := setupHarnessAdapters()
	if projectExists {
		if err := validateSetupCommittedConfig(existingProject, options.projectRoot, adapters); err != nil {
			return setupState{}, err
		}
	}
	capabilities, err := detectSetupAgents(adapters)
	if err != nil {
		return setupState{}, err
	}
	agents, err := chooseSetupAgents(options, capabilities, existingProject, projectExists)
	if err != nil {
		return setupState{}, err
	}
	defaultSession, err := chooseSetupSession(options, existingProject, projectExists)
	if err != nil {
		return setupState{}, err
	}
	availableLaunchers := detectSetupLaunchers()
	launcherPreference, err := chooseSetupLaunchers(options, availableLaunchers, existingLocal, localExists)
	if err != nil {
		return setupState{}, err
	}

	projectConfig := launch.ProjectConfig{
		Schema: launch.ProjectConfigSchema, DefaultSession: defaultSession, Agents: agents,
		Layout: launch.LayoutIntent{Type: options.layout},
	}
	projectData, err := launch.MarshalProjectConfig(projectConfig)
	if err != nil {
		return setupState{}, err
	}
	if projectExists {
		existingCanonical, marshalErr := launch.MarshalProjectConfig(existingProject)
		if marshalErr != nil {
			return setupState{}, marshalErr
		}
		if bytes.Equal(projectData, existingCanonical) {
			projectData = existingProjectData
		}
	}
	localConfig := launch.LocalConfig{Schema: launch.LocalConfigSchema, LauncherPreference: launcherPreference}
	localData, err := launch.MarshalLocalConfig(localConfig)
	if err != nil {
		return setupState{}, err
	}
	if localExists {
		existingCanonical, marshalErr := launch.MarshalLocalConfig(existingLocal)
		if marshalErr != nil {
			return setupState{}, marshalErr
		}
		if bytes.Equal(localData, existingCanonical) {
			localData = existingLocalData
		}
	}
	if !amqrcExists {
		value, marshalErr := json.MarshalIndent(amqrc{Root: root}, "", "  ")
		if marshalErr != nil {
			return setupState{}, marshalErr
		}
		amqrcData = append(value, '\n')
	}

	handles := projectAgentHandles(agents)
	baseConfig, currentBaseData, currentAgents, err := setupBaseConfig(root, handles)
	if err != nil {
		return setupState{}, err
	}
	baseConfigData, err := marshalSetupBaseConfig(baseConfig)
	if err != nil {
		return setupState{}, err
	}
	unionConfig := baseConfig
	unionConfig.Agents = sortedUnion(currentAgents, handles)
	unionConfigData, err := marshalSetupBaseConfig(unionConfig)
	if err != nil {
		return setupState{}, err
	}

	gitignoreData, gitignoreChanged, err := setupGitignoreData(root, options.noGitignore)
	if err != nil {
		return setupState{}, err
	}
	needsProvision, err := setupNeedsProvisioning(root, defaultSession, handles)
	if err != nil {
		return setupState{}, err
	}
	state := setupState{
		projectRoot: options.projectRoot, queueRoot: root, projectConfig: projectConfig,
		localConfig: localConfig, projectData: projectData, localData: localData,
		amqrcData: amqrcData, gitignoreData: gitignoreData,
		baseConfigData: baseConfigData, unionConfigData: unionConfigData,
		needsProvision: needsProvision, available: availableLaunchers, noGitignore: options.noGitignore,
	}
	state.addChange(needsProvision, filepath.Join(root, defaultSession), "ensure queue and roster mailboxes")
	state.addChange(!bytes.Equal(currentBaseData, unionConfigData), filepath.Join(root, "meta", "config.json"), "update compatible roster")
	state.addChange(fileDiffers(setupConfigPath, projectData), setupConfigPath, "write project declaration")
	state.addChange(!bytes.Equal(unionConfigData, baseConfigData), filepath.Join(root, "meta", "config.json"), "finalize roster")
	state.addChange(fileDiffers(setupLocalConfigPath, localData), setupLocalConfigPath, "write local preferences")
	state.addChange(gitignoreChanged, ".gitignore", "append AMQ ignore entries")
	state.addChange(!amqrcExists, ".amqrc", "activate project root discovery")
	return state, nil
}

func (state *setupState) addChange(condition bool, path, action string) {
	if condition {
		state.changes = append(state.changes, setupChange{Path: path, Action: action})
	}
}

func (state setupState) preview() (setupPreview, error) {
	preview := setupPreview{
		ProjectRoot: state.projectRoot, QueueRoot: state.queueRoot,
		DefaultSession:     state.projectConfig.DefaultSession,
		Agents:             append([]launch.ProjectAgentConfig(nil), state.projectConfig.Agents...),
		Layout:             state.projectConfig.Layout,
		LauncherPreference: append([]string(nil), state.localConfig.LauncherPreference...),
		AvailableLaunchers: append([]string(nil), state.available...),
		Changes:            append(make([]setupChange, 0, len(state.changes)), state.changes...),
	}
	digest, err := setupPreviewDigest(preview)
	if err != nil {
		return setupPreview{}, err
	}
	preview.Digest = digest
	return preview, nil
}

func setupPreviewDigest(preview setupPreview) (string, error) {
	canonical, err := json.Marshal(struct {
		Version            int                         `json:"version"`
		ProjectRoot        string                      `json:"project_root"`
		QueueRoot          string                      `json:"queue_root"`
		DefaultSession     string                      `json:"default_session"`
		Agents             []launch.ProjectAgentConfig `json:"agents"`
		Layout             launch.LayoutIntent         `json:"layout"`
		LauncherPreference []string                    `json:"launcher_preference"`
		Changes            []setupChange               `json:"changes"`
	}{
		Version: 1, ProjectRoot: preview.ProjectRoot, QueueRoot: preview.QueueRoot,
		DefaultSession: preview.DefaultSession, Agents: preview.Agents, Layout: preview.Layout,
		LauncherPreference: preview.LauncherPreference, Changes: preview.Changes,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("sha256:%x", sum), nil
}

func validSetupDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	for _, char := range strings.TrimPrefix(value, "sha256:") {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func printSetupPreview(preview setupPreview) error {
	if err := writeStdout("Set up AMQ in %s\n\n", preview.ProjectRoot); err != nil {
		return err
	}
	if err := writeStdout("Agents: %s\n", strings.Join(projectAgentHandles(preview.Agents), ", ")); err != nil {
		return err
	}
	if err := writeStdout("Default session: %s\n", preview.DefaultSession); err != nil {
		return err
	}
	if err := writeStdout("Launcher preference: %s\n", strings.Join(preview.LauncherPreference, ", ")); err != nil {
		return err
	}
	if err := writeStdout("Queue root: %s\nLayout: %s\n\nChanges:\n", preview.QueueRoot, preview.Layout.Type); err != nil {
		return err
	}
	if len(preview.Changes) == 0 {
		if err := writeStdoutLine("  (none)"); err != nil {
			return err
		}
	} else {
		for _, change := range preview.Changes {
			if err := writeStdout("  - %s: %s\n", change.Path, change.Action); err != nil {
				return err
			}
		}
	}
	return writeStdout("\nApproval digest: %s\n", preview.Digest)
}

func detectSetupAgents(adapters []launch.HarnessAdapter) ([]launch.AdapterCapabilities, error) {
	capabilities := make([]launch.AdapterCapabilities, 0, len(adapters))
	for _, adapter := range adapters {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		capability := adapter.Capabilities(ctx)
		cancel()
		if err := launch.ValidateAdapterCapabilities(adapter, capability); err != nil {
			return nil, err
		}
		if capability.Available && capability.Fresh {
			capabilities = append(capabilities, capability)
		}
	}
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i].Provider < capabilities[j].Provider })
	return capabilities, nil
}

func validateSetupCommittedConfig(cfg launch.ProjectConfig, projectRoot string, adapters []launch.HarnessAdapter) error {
	byName := make(map[string]launch.HarnessAdapter, len(adapters))
	for _, adapter := range adapters {
		byName[adapter.Name()] = adapter
	}
	for _, agent := range cfg.Agents {
		adapter, ok := byName[agent.Adapter]
		if !ok {
			return &setupConfigRefusalError{
				Handle: agent.Handle, Adapter: agent.Adapter,
				Err: fmt.Errorf("adapter is not supported by this AMQ build"),
			}
		}
		if err := launch.ValidateCommittedConfig(adapter, launch.CommittedConfigRequest{
			ProjectRoot: projectRoot,
			Cwd:         agent.Cwd,
			Args:        append([]string(nil), agent.Command[1:]...),
			EnvOverlay:  agent.Env,
		}); err != nil {
			return &setupConfigRefusalError{Handle: agent.Handle, Adapter: agent.Adapter, Err: err}
		}
	}
	return nil
}

func chooseSetupAgents(options setupOptions, detected []launch.AdapterCapabilities, existing launch.ProjectConfig, exists bool) ([]launch.ProjectAgentConfig, error) {
	available := make(map[string]launch.AdapterCapabilities, len(detected))
	names := make([]string, 0, len(detected))
	for _, capability := range detected {
		available[capability.Provider] = capability
		names = append(names, capability.Provider)
	}
	existingByHandle := make(map[string]launch.ProjectAgentConfig, len(existing.Agents))
	for _, agent := range existing.Agents {
		existingByHandle[agent.Handle] = agent
		if _, ok := available[agent.Handle]; !ok {
			names = append(names, agent.Handle)
		}
	}
	names = dedupeStrings(names)
	sort.Strings(names)
	var selected []string
	switch {
	case options.agentsExplicit:
		var err error
		selected, err = parseCoopInitAgents(options.agents)
		if err != nil {
			return nil, err
		}
	case exists && options.nonInteractive:
		return append([]launch.ProjectAgentConfig(nil), existing.Agents...), nil
	case options.nonInteractive:
		selected = names
	default:
		if len(names) == 0 {
			return nil, fmt.Errorf("no supported agent CLI detected")
		}
		defaultSelection := names
		if exists {
			defaultSelection = projectAgentHandles(existing.Agents)
		}
		line, err := setupPromptLine(options.input, "Agents (comma-separated)", strings.Join(defaultSelection, ","))
		if err != nil {
			return nil, err
		}
		selected, err = parseCoopInitAgents(line)
		if err != nil {
			return nil, err
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no supported agent CLI detected")
	}
	agents := make([]launch.ProjectAgentConfig, 0, len(selected))
	for _, name := range selected {
		if agent, ok := existingByHandle[name]; ok {
			agents = append(agents, agent)
			continue
		}
		if _, ok := available[name]; !ok {
			return nil, UsageError("agent %q is not available through a supported adapter probe", name)
		}
		agents = append(agents, launch.ProjectAgentConfig{
			Handle: name, Adapter: name, Command: []string{name}, ResumePolicy: launch.ResumeEnabled,
		})
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Handle < agents[j].Handle })
	return agents, nil
}

func chooseSetupSession(options setupOptions, existing launch.ProjectConfig, exists bool) (string, error) {
	value := defaultSessionName
	if exists {
		value = existing.DefaultSession
	}
	if options.sessionExplicit {
		value = strings.TrimSpace(options.defaultSession)
	} else if !options.nonInteractive {
		line, err := setupPromptLine(options.input, "Default session", value)
		if err != nil {
			return "", err
		}
		value = line
	}
	if err := validateSessionName(value); err != nil {
		return "", err
	}
	return value, nil
}

func chooseSetupLaunchers(options setupOptions, detected []string, existing launch.LocalConfig, exists bool) ([]string, error) {
	preference := append([]string(nil), detected...)
	if exists {
		preference = append([]string(nil), existing.LauncherPreference...)
	}
	var raw string
	if options.launchersExplicit {
		raw = options.launchers
	} else if !options.nonInteractive {
		line, err := setupPromptLine(options.input, "Launcher preference (comma-separated)", strings.Join(preference, ","))
		if err != nil {
			return nil, err
		}
		raw = line
	}
	if raw != "" {
		preference = splitSetupList(raw)
	}
	if !slices.Contains(preference, launch.LauncherCommands) {
		preference = append(preference, launch.LauncherCommands)
	}
	preference = dedupeStrings(preference)
	cfg := launch.LocalConfig{Schema: launch.LocalConfigSchema, LauncherPreference: preference}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return preference, nil
}

func detectSetupLaunchers() []string {
	result := make([]string, 0, 4)
	for _, name := range []string{launch.LauncherCMux, launch.LauncherGhostty, launch.LauncherTMux} {
		if setupLauncherAvailable(name) {
			result = append(result, name)
		}
	}
	return append(result, launch.LauncherCommands)
}

func setupLauncherAvailable(name string) bool {
	if name == launch.LauncherCMux {
		return setupCmuxAvailable()
	}
	_, err := setupLookPath(name)
	return err == nil
}

func setupPromptLine(reader *bufio.Reader, label, defaultValue string) (string, error) {
	if reader == nil {
		return "", fmt.Errorf("interactive setup reader is unavailable")
	}
	if err := writeStdout("%s [%s]: ", label, defaultValue); err != nil {
		return "", err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultValue, nil
	}
	return line, nil
}

func setupConfirm(reader *bufio.Reader, prompt string) (bool, error) {
	if reader == nil {
		return false, fmt.Errorf("interactive setup reader is unavailable")
	}
	if err := writeStdout("%s [Y/n]: ", prompt); err != nil {
		return false, err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes", nil
}

func splitSetupList(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func setupRoot(requested string, explicit bool, projectRoot string) (string, []byte, bool, error) {
	result, err := findAndLoadAmqrc()
	if err == nil {
		if !sameTreeIdentity(result.Dir, projectRoot) {
			return "", nil, false, fmt.Errorf("project setup resolved parent .amqrc at %s", result.Path)
		}
		raw, readErr := os.ReadFile(result.Path)
		if readErr != nil {
			return "", nil, false, readErr
		}
		var fields map[string]json.RawMessage
		if jsonErr := json.Unmarshal(raw, &fields); jsonErr != nil {
			return "", nil, false, jsonErr
		}
		if _, conflict := fields["default_session"]; conflict {
			return "", nil, false, &launch.ConfigAuthorityConflictError{Path: ".amqrc", Field: "default_session"}
		}
		if explicit && requested != result.Config.Root {
			return "", nil, false, fmt.Errorf(".amqrc already selects root %q", result.Config.Root)
		}
		return result.Config.Root, raw, true, nil
	}
	if !errors.Is(err, errAmqrcNotFound) {
		return "", nil, false, err
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil, false, UsageError("--root must not be blank")
	}
	return requested, nil, false, nil
}

func validateSetupConfigDirectory() error {
	info, err := os.Lstat(".amq")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf(".amq must be a real directory")
	}
	return nil
}

func loadOptionalProjectConfig(path string) (launch.ProjectConfig, []byte, bool, error) {
	data, exists, err := readOptionalRegular(path)
	if err != nil || !exists {
		return launch.ProjectConfig{}, nil, exists, err
	}
	cfg, err := launch.ParseProjectConfig(data)
	return cfg, data, true, err
}

func loadOptionalLocalConfig(path string) (launch.LocalConfig, []byte, bool, error) {
	data, exists, err := readOptionalRegular(path)
	if err != nil || !exists {
		return launch.LocalConfig{}, nil, exists, err
	}
	cfg, err := launch.ParseLocalConfig(path, data)
	return cfg, data, true, err
}

func readOptionalRegular(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s must be a regular file", path)
	}
	data, err := os.ReadFile(path)
	return data, true, err
}

func setupBaseConfig(root string, agents []string) (config.Config, []byte, []string, error) {
	path := filepath.Join(root, "meta", "config.json")
	data, exists, err := readOptionalRegular(path)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	cfg := config.Config{Version: format.CurrentVersion, CreatedUTC: time.Now().UTC().Format(time.RFC3339), Agents: agents}
	var current []string
	if exists {
		loaded, loadErr := config.LoadConfig(path)
		if loadErr != nil {
			return config.Config{}, nil, nil, loadErr
		}
		for _, agent := range loaded.Agents {
			if err := fsq.ValidateHandle(agent); err != nil {
				return config.Config{}, nil, nil, fmt.Errorf("invalid agent in existing config: %w", err)
			}
		}
		cfg.CreatedUTC = loaded.CreatedUTC
		current = append([]string(nil), loaded.Agents...)
	}
	return cfg, data, current, nil
}

func marshalSetupBaseConfig(cfg config.Config) ([]byte, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func sortedUnion(left, right []string) []string {
	result := dedupeStrings(append(append([]string(nil), left...), right...))
	sort.Strings(result)
	return result
}

func projectAgentHandles(agents []launch.ProjectAgentConfig) []string {
	result := make([]string, 0, len(agents))
	for _, agent := range agents {
		result = append(result, agent.Handle)
	}
	return result
}

func setupNeedsProvisioning(root, session string, agents []string) (bool, error) {
	paths := []string{
		filepath.Join(root, "agents"), filepath.Join(root, "threads"), filepath.Join(root, "meta"),
		filepath.Join(root, session, "agents"), filepath.Join(root, session, "threads"), filepath.Join(root, session, "meta"),
	}
	for _, base := range []string{root, filepath.Join(root, session)} {
		for _, agent := range agents {
			for _, leaf := range fsq.RequiredMailboxLeaves() {
				paths = append(paths, filepath.Join(base, "agents", agent, filepath.FromSlash(string(leaf))))
			}
		}
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

func setupGitignoreData(root string, disabled bool) ([]byte, bool, error) {
	if disabled {
		return nil, false, nil
	}
	data, exists, err := readOptionalRegular(".gitignore")
	if err != nil {
		return nil, false, err
	}
	patterns := []string{setupLocalConfigPath}
	if !filepath.IsAbs(root) {
		patterns = append([]string{strings.TrimSuffix(filepath.ToSlash(root), "/") + "/"}, patterns...)
	}
	missing := make([]string, 0, len(patterns))
	lines := strings.Split(string(data), "\n")
	for _, pattern := range patterns {
		found := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
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
		return data, false, nil
	}
	var suffix strings.Builder
	if exists && len(data) > 0 {
		suffix.WriteByte('\n')
	}
	suffix.WriteString("# Agent Message Queue local state\n")
	suffix.WriteString(strings.Join(missing, "\n"))
	suffix.WriteByte('\n')
	return append(append([]byte(nil), data...), []byte(suffix.String())...), true, nil
}

func fileDiffers(path string, desired []byte) bool {
	current, exists, err := readOptionalRegular(path)
	return err != nil || !exists || !bytes.Equal(current, desired)
}

func commitSetup(state setupState) ([]string, error) {
	written := make([]string, 0, len(state.changes))
	step := func(name string) error {
		written = append(written, name)
		if setupCommitStepHook != nil {
			return setupCommitStepHook(name)
		}
		return nil
	}
	if state.needsProvision {
		if err := provisionCoopBaseAndSession(state.queueRoot, state.projectConfig.DefaultSession, projectAgentHandles(state.projectConfig.Agents)); err != nil {
			return written, err
		}
		if err := step("provision"); err != nil {
			return written, err
		}
	}
	baseConfigPath := filepath.Join(state.queueRoot, "meta", "config.json")
	if changed, err := setupWriteAtomicIfChanged(baseConfigPath, state.unionConfigData, 0o600); err != nil {
		return written, err
	} else if changed {
		if err := step("roster_compatible"); err != nil {
			return written, err
		}
	}
	if changed, err := setupWriteAtomicIfChanged(setupConfigPath, state.projectData, 0o644); err != nil {
		return written, err
	} else if changed {
		if err := step("project_config"); err != nil {
			return written, err
		}
	}
	if changed, err := setupWriteAtomicIfChanged(baseConfigPath, state.baseConfigData, 0o600); err != nil {
		return written, err
	} else if changed {
		if err := step("roster_final"); err != nil {
			return written, err
		}
	}
	if changed, err := setupWriteAtomicIfChanged(setupLocalConfigPath, state.localData, 0o600); err != nil {
		return written, err
	} else if changed {
		if err := step("local_config"); err != nil {
			return written, err
		}
	}
	if !state.noGitignore && len(state.gitignoreData) > 0 {
		if changed, err := setupWriteAtomicIfChanged(".gitignore", state.gitignoreData, 0o644); err != nil {
			return written, err
		} else if changed {
			if err := step("gitignore"); err != nil {
				return written, err
			}
		}
	}
	if changed, err := setupWriteAtomicIfChanged(".amqrc", state.amqrcData, 0o644); err != nil {
		return written, err
	} else if changed {
		if err := step("amqrc"); err != nil {
			return written, err
		}
	}
	return written, nil
}

func setupWriteAtomicIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	current, exists, err := readOptionalRegular(path)
	if err != nil {
		return false, err
	}
	if exists && bytes.Equal(current, data) {
		return false, nil
	}
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), data, mode)
	return err == nil, err
}
