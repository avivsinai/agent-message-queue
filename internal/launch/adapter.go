package launch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

const (
	ClaudeProvider = "claude"
	CodexProvider  = "codex"
	CursorProvider = "cursor-agent"
)

// ProviderForExecutable maps a configured executable basename to the stable
// adapter/provider identity. Cursor's current CLI is "agent"; cursor-agent
// remains a supported legacy executable alias.
func ProviderForExecutable(executable string) string {
	base := strings.ToLower(filepath.Base(executable))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case ClaudeProvider, CodexProvider:
		return base
	case "agent", CursorProvider:
		return CursorProvider
	default:
		return ""
	}
}

// HarnessAdapter owns conversation identity and produces backend-ready plans.
// It must not create, inspect, focus, or close terminal resources.
type HarnessAdapter interface {
	Name() string
	Mode() AdapterMode
	CommittedEnvKeys() []string
	Capabilities(context.Context) AdapterCapabilities
	PlanFresh(PlanRequest) (AgentPlan, error)
	PlanResume(ResumeRequest) (AgentPlan, error)
	CaptureIdentity(CaptureRequest) CaptureResult
}

type AdapterCapabilities struct {
	Provider        string      `json:"provider"`
	Mode            AdapterMode `json:"mode"`
	Available       bool        `json:"available"`
	Executable      string      `json:"executable,omitempty"`
	ProviderVersion string      `json:"provider_version,omitempty"`
	Fresh           bool        `json:"fresh"`
	Resume          bool        `json:"resume"`
	Capture         bool        `json:"capture"`
	PreSpawnAcquire bool        `json:"pre_spawn_acquire"`
	Reason          string      `json:"reason,omitempty"`
}

type ConfigOverrideCapability struct {
	Key           string
	AllowedValues []string
}

type StaticInputCapabilities struct {
	GrammarVersion          int
	VerifiedProviderVersion string
	AllowedArgumentForms    []string
	ConfigOverrides         []ConfigOverrideCapability
	InitialInputKinds       []InitialInputKind
}

// ProviderStaticInputCapabilities is the single public-compiler projection of
// the same deny-by-default tables used by validation.
func ProviderStaticInputCapabilities(provider string) StaticInputCapabilities {
	switch provider {
	case ClaudeProvider:
		return StaticInputCapabilities{
			GrammarVersion: 1, VerifiedProviderVersion: "2.1.233",
			AllowedArgumentForms: []string{"--allowedTools"}, InitialInputKinds: []InitialInputKind{InitialInputArgument},
		}
	case CodexProvider:
		values := slices.Clone(codexReasoningEffortValues)
		return StaticInputCapabilities{
			GrammarVersion: 1, VerifiedProviderVersion: codexCaptureVersion,
			AllowedArgumentForms: []string{"-c"},
			ConfigOverrides:      []ConfigOverrideCapability{{Key: "model_reasoning_effort", AllowedValues: values}},
			InitialInputKinds:    []InitialInputKind{InitialInputArgument},
		}
	default:
		return StaticInputCapabilities{AllowedArgumentForms: []string{}, ConfigOverrides: []ConfigOverrideCapability{}, InitialInputKinds: []InitialInputKind{}}
	}
}

type PlanRequest struct {
	Handle        string
	ProjectRoot   string
	SessionRoot   string
	AMQExecutable string
	Cwd           string
	// AllowExternalCwd is reserved for the public intent compiler. Public
	// intents can name an absolute sibling worktree after that path is
	// canonicalized and physically identified. Committed project config keeps
	// the historical project-contained rule.
	AllowExternalCwd bool
	LaunchNonce      string
	ResumePolicy     ResumePolicy
	CommittedArgs    []string
	BypassArgs       []string
	EnvOverlay       map[string]string
	InitialInput     *InitialInputRequest
	Wrapper          *Wrapper
}

type InitialInputRequest struct {
	Kind   InitialInputKind
	Value  string
	SHA256 string
}

func appendInitialInput(plan *AgentPlan, input *InitialInputRequest) error {
	if input == nil {
		return nil
	}
	if input.Kind != InitialInputArgument {
		return fmt.Errorf("initial input kind %q is unsupported", input.Kind)
	}
	if !validDigest(input.SHA256) {
		return fmt.Errorf("initial input is invalid")
	}
	if err := validateInitialInputText(input.Value); err != nil {
		return err
	}
	plan.Argv = append(plan.Argv, input.Value)
	plan.InitialInput = &PlannedInitialInput{Kind: input.Kind, SHA256: input.SHA256, ArgvIndex: len(plan.Argv) - 1}
	return nil
}

func validateInitialInputText(value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("initial input must not begin with '-'")
	}
	for _, r := range value {
		if r <= 0x1f || r == 0x7f {
			return fmt.Errorf("initial input contains control character")
		}
	}
	return nil
}

type ResumeRequest struct {
	PlanRequest
	Conversation ConversationIdentity
}

type ConversationIdentity struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// CommittedConfigRequest is the static, repository-controlled subset of an
// adapter plan. Validation does not require the provider executable to be
// installed, so setup can reject unsafe committed carriers on any machine.
type CommittedConfigRequest struct {
	ProjectRoot string
	Cwd         string
	Args        []string
	EnvOverlay  map[string]string
}

// CommittedConfigValidator is implemented by adapters that can own committed
// argv, environment, and cwd validation independently from live capability
// probing and per-launch identity generation.
type CommittedConfigValidator interface {
	ValidateCommittedConfig(CommittedConfigRequest) error
}

func ValidateCommittedConfig(adapter HarnessAdapter, request CommittedConfigRequest) error {
	validator, ok := adapter.(CommittedConfigValidator)
	if !ok {
		return fmt.Errorf("adapter %s does not validate committed configuration", adapter.Name())
	}
	return validator.ValidateCommittedConfig(request)
}

// ValidateStaticProviderInput validates caller-owned provider argv and
// environment without resolving runtime identity or generating a plan. The
// executable selects one built-in adapter by basename. Operator bypass flags
// remain explicit static input, but are accepted only from that adapter's
// fixed allow-list.
func ValidateStaticProviderInput(executable string, args []string, env map[string]string) (string, error) {
	if executable == "" || strings.TrimSpace(executable) != executable || strings.ContainsRune(executable, 0) {
		return "", fmt.Errorf("executable is invalid")
	}
	provider := ProviderForExecutable(executable)
	var envRules map[string]valueRule
	var argRules map[string]argumentRule
	var bypassAllowed map[string]struct{}
	switch provider {
	case ClaudeProvider:
		envRules, argRules, bypassAllowed = claudeEnvRules(), claudeArgRules(), claudeBypassArgs()
	case CodexProvider:
		envRules, argRules, bypassAllowed = codexEnvRules(), codexArgRules(), codexBypassArgs()
	case CursorProvider:
		envRules, argRules, bypassAllowed = cursorEnvRules(), cursorArgRules(), cursorBypassArgs()
	default:
		return "", fmt.Errorf("executable %q does not select an adapter-known provider", executable)
	}
	if err := validateStaticProviderArgs(args, argRules, bypassAllowed); err != nil {
		return "", err
	}
	switch provider {
	case ClaudeProvider:
		if err := validateSingleStaticArgument(args, "--allowedTools"); err != nil {
			return "", err
		}
	case CodexProvider:
		if err := validateCodexConfigOverrides(args); err != nil {
			return "", err
		}
	}
	if provider == CursorProvider {
		if err := validateCursorBypassArgs(args, true); err != nil {
			return "", err
		}
	}
	if err := validateCommittedEnv(env, envRules); err != nil {
		return "", err
	}
	return provider, nil
}

func validateSingleStaticArgument(args []string, name string) error {
	count := 0
	for _, arg := range args {
		if arg == name {
			count++
		}
	}
	if count > 1 {
		return fmt.Errorf("static argument %q is duplicated", name)
	}
	return nil
}

// PartitionStaticProviderArgs separates ordinary committed arguments from the
// adapter's explicit operator-bypass arguments. Callers must first validate the
// complete input with ValidateStaticProviderInput.
func PartitionStaticProviderArgs(provider string, args []string) (committed, bypass []string, err error) {
	var argRules map[string]argumentRule
	var bypassAllowed map[string]struct{}
	switch provider {
	case ClaudeProvider:
		argRules, bypassAllowed = claudeArgRules(), claudeBypassArgs()
	case CodexProvider:
		argRules, bypassAllowed = codexArgRules(), codexBypassArgs()
	case CursorProvider:
		argRules, bypassAllowed = cursorArgRules(), cursorBypassArgs()
	default:
		return nil, nil, fmt.Errorf("unknown provider %q", provider)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if _, ok := bypassAllowed[arg]; ok {
			bypass = append(bypass, arg)
			continue
		}
		rule, ok := argRules[arg]
		if !ok {
			return nil, nil, fmt.Errorf("static argument %q is not allowed by adapter grammar", arg)
		}
		committed = append(committed, arg)
		if rule.value {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("static argument %q requires a value", arg)
			}
			i++
			committed = append(committed, args[i])
		}
	}
	return committed, bypass, nil
}

func validateStaticProviderArgs(args []string, rules map[string]argumentRule, bypassAllowed map[string]struct{}) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if _, ok := bypassAllowed[arg]; ok {
			continue
		}
		rule, ok := rules[arg]
		if !ok {
			return fmt.Errorf("static argument %q is not allowed by adapter grammar", arg)
		}
		if !rule.value {
			continue
		}
		if i+1 >= len(args) {
			return fmt.Errorf("static argument %q requires a value", arg)
		}
		i++
		if rule.validate != nil && !rule.validate(args[i]) {
			return fmt.Errorf("static argument %q has invalid value %q", arg, args[i])
		}
	}
	return nil
}

type commandProbe interface {
	LookPath(string) (string, error)
	Output(context.Context, string, ...string) ([]byte, error)
}

type systemCommandProbe struct{}

func (systemCommandProbe) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (systemCommandProbe) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func ValidateAdapterPlan(adapter HarnessAdapter, plan AgentPlan) error {
	if adapter == nil {
		return fmt.Errorf("missing harness adapter")
	}
	if plan.AdapterMode != adapter.Mode() {
		return fmt.Errorf("adapter %s declares %q but built %q plan", adapter.Name(), adapter.Mode(), plan.AdapterMode)
	}
	return plan.Validate()
}

func ValidateAdapterCapabilities(adapter HarnessAdapter, capabilities AdapterCapabilities) error {
	if adapter == nil {
		return fmt.Errorf("missing harness adapter")
	}
	if capabilities.Provider != adapter.Name() {
		return fmt.Errorf("adapter %s reported provider %q", adapter.Name(), capabilities.Provider)
	}
	if capabilities.Mode != adapter.Mode() {
		return fmt.Errorf("adapter %s declares %q but reported %q mode", adapter.Name(), adapter.Mode(), capabilities.Mode)
	}
	if capabilities.Capture && adapter.Mode() != AdapterModeCapture {
		return fmt.Errorf("adapter %s capture capability conflicts with %q mode", adapter.Name(), adapter.Mode())
	}
	if capabilities.PreSpawnAcquire && (!capabilities.Capture || adapter.Mode() != AdapterModeCapture) {
		return fmt.Errorf("adapter %s pre-spawn acquisition requires capture mode", adapter.Name())
	}
	return nil
}

func validatePlanRequest(request PlanRequest, executable, provider string, envRules map[string]valueRule, argRules map[string]argumentRule, bypassAllowed map[string]struct{}) (string, error) {
	if !validUUID(request.LaunchNonce) {
		return "", fmt.Errorf("launch nonce must be a UUID")
	}
	if request.ResumePolicy != ResumeEnabled && request.ResumePolicy != ResumeFresh && request.ResumePolicy != ResumeDisabled {
		return "", fmt.Errorf("invalid resume policy %q", request.ResumePolicy)
	}
	resolvedExecutable, err := validateKnownExecutable(executable, request.ProjectRoot, provider)
	if err != nil {
		return "", err
	}
	if provider == CodexProvider {
		if err := validateCodexConfigOverrides(request.CommittedArgs); err != nil {
			return "", err
		}
	}
	if err := validateCommittedArgs(request.CommittedArgs, argRules); err != nil {
		return "", err
	}
	for _, arg := range request.BypassArgs {
		if _, ok := bypassAllowed[arg]; !ok {
			return "", fmt.Errorf("unrecognized operator bypass argument %q", arg)
		}
	}
	if err := validateCommittedEnv(request.EnvOverlay, envRules); err != nil {
		return "", err
	}
	if err := validatePlanWorkingDirectory(request); err != nil {
		return "", err
	}
	return resolvedExecutable, nil
}

func validatePlanWorkingDirectory(request PlanRequest) error {
	if !request.AllowExternalCwd {
		return validateWorkingDirectory(request.Cwd, request.ProjectRoot)
	}
	if !filepath.IsAbs(request.Cwd) {
		return fmt.Errorf("external working directory must be absolute")
	}
	resolved, err := resolvedPath(request.Cwd)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat working directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory is not a directory")
	}
	return nil
}

func validateCommittedConfig(request CommittedConfigRequest, envRules map[string]valueRule, argRules map[string]argumentRule) error {
	if err := validateCommittedArgs(request.Args, argRules); err != nil {
		return err
	}
	if err := validateCommittedEnv(request.EnvOverlay, envRules); err != nil {
		return err
	}
	cwd := request.Cwd
	if strings.TrimSpace(cwd) == "" {
		cwd = request.ProjectRoot
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(request.ProjectRoot, cwd)
	}
	return validateWorkingDirectory(cwd, request.ProjectRoot)
}

func cloneEnv(overlay map[string]string) map[string]string {
	if overlay == nil {
		return nil
	}
	clone := make(map[string]string, len(overlay))
	for key, value := range overlay {
		clone[key] = value
	}
	return clone
}

func validateKnownExecutable(executable, projectRoot, provider string) (string, error) {
	if !filepath.IsAbs(executable) {
		lookedUp, err := exec.LookPath(executable)
		if err != nil {
			return "", fmt.Errorf("resolve %s executable: %w", provider, err)
		}
		executable, err = filepath.Abs(lookedUp)
		if err != nil {
			return "", fmt.Errorf("make %s executable absolute: %w", provider, err)
		}
	} else {
		executable = filepath.Clean(executable)
	}
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable: %w", provider, err)
	}
	if ProviderForExecutable(executable) != provider {
		return "", fmt.Errorf("adapter %s cannot execute %q", provider, resolvedExecutable)
	}
	if strings.TrimSpace(projectRoot) == "" {
		return "", fmt.Errorf("project root is required for executable validation")
	}
	resolvedProject, err := resolvedPath(projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	if pathWithin(resolvedExecutable, resolvedProject) {
		return "", fmt.Errorf("%s executable resolves inside the project", provider)
	}
	info, err := os.Stat(resolvedExecutable)
	if err != nil {
		return "", fmt.Errorf("stat %s executable: %w", provider, err)
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return "", fmt.Errorf("%s executable is not an executable regular file", provider)
	}
	// Keep the stable pre-resolution path in the plan and semantic digest. The
	// physical target is used only for the security and executability checks
	// above; persisting it would freeze version-manager symlink targets and
	// change trust digests on every tool update.
	return executable, nil
}

// validateExecutableContainment checks the property that must be established
// before an adapter capability probe: a provider executable must not resolve
// into the project. Availability and executable shape remain capability/plan
// concerns; a missing command cannot be a project-contained process.
func validateExecutableContainment(executable, projectRoot, provider string) error {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(projectRoot) == "" {
		return nil
	}
	project, err := resolvedPath(projectRoot)
	if err != nil {
		return fmt.Errorf("resolve project root for %s executable: %w", provider, err)
	}
	candidate := executable
	if !filepath.IsAbs(candidate) {
		lookedUp, lookupErr := exec.LookPath(candidate)
		if lookupErr != nil {
			return nil
		}
		candidate = lookedUp
	}
	requested, err := filepath.Abs(candidate)
	if err != nil {
		return fmt.Errorf("make %s executable absolute: %w", provider, err)
	}
	if pathWithin(filepath.Clean(requested), project) {
		return &LaunchPathError{Code: ProviderProjectContainedCode, Path: executable}
	}
	resolved, err := resolvedPath(requested)
	if err != nil {
		// An unavailable executable is reported by the adapter capability
		// probe. There is no executable identity to classify as contained.
		return nil
	}
	if pathWithin(resolved, project) {
		return &LaunchPathError{Code: ProviderProjectContainedCode, Path: executable}
	}
	return nil
}

func validateWorkingDirectory(cwd, projectRoot string) error {
	resolvedProject, err := resolvedPath(projectRoot)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	resolvedCwd, err := resolvedPath(cwd)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	if !pathWithin(resolvedCwd, resolvedProject) {
		return fmt.Errorf("working directory must be inside the project")
	}
	info, err := os.Stat(resolvedCwd)
	if err != nil {
		return fmt.Errorf("stat working directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory is not a directory")
	}
	return nil
}

type argumentRule struct {
	value    bool
	validate valueRule
}

func validateCommittedArgs(args []string, rules map[string]argumentRule) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		rule, ok := rules[arg]
		if !ok {
			return fmt.Errorf("committed argument %q is not allowed by adapter grammar", arg)
		}
		if !rule.value {
			continue
		}
		if i+1 >= len(args) {
			return fmt.Errorf("committed argument %q requires a value", arg)
		}
		i++
		if rule.validate != nil && !rule.validate(args[i]) {
			return fmt.Errorf("committed argument %q has invalid value %q", arg, args[i])
		}
	}
	return nil
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func safeArgumentValue(value string) bool {
	return !strings.HasPrefix(value, "-") && safeEnvironmentValue(value)
}

func validUUID(value string) bool {
	return uuidPattern.MatchString(value)
}

func validUUIDv7(value string) bool {
	if !validUUID(value) || value[14] != '7' {
		return false
	}
	variant := value[19]
	return variant == '8' || variant == '9' || variant == 'a' || variant == 'A' || variant == 'b' || variant == 'B'
}

func validateConversation(identity ConversationIdentity, provider string) error {
	if identity.Provider != provider {
		return fmt.Errorf("conversation provider %q does not match adapter %q", identity.Provider, provider)
	}
	if !validUUID(identity.ID) {
		return fmt.Errorf("conversation ID must be a UUID")
	}
	return nil
}
