package launch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	ClaudeProvider = "claude"
	CodexProvider  = "codex"
)

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
	Reason          string      `json:"reason,omitempty"`
}

type PlanRequest struct {
	Handle        string
	ProjectRoot   string
	Cwd           string
	LaunchNonce   string
	ResumePolicy  ResumePolicy
	CommittedArgs []string
	BypassArgs    []string
	EnvOverlay    map[string]string
}

type ResumeRequest struct {
	PlanRequest
	Conversation ConversationIdentity
}

type ConversationIdentity struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
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
	if err := validateWorkingDirectory(request.Cwd, request.ProjectRoot); err != nil {
		return "", err
	}
	return resolvedExecutable, nil
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
		resolvedPath, err := exec.LookPath(executable)
		if err != nil {
			return "", fmt.Errorf("resolve %s executable: %w", provider, err)
		}
		executable = resolvedPath
	}
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable: %w", provider, err)
	}
	base := strings.ToLower(filepath.Base(executable))
	if runtime.GOOS == "windows" {
		base = strings.TrimSuffix(base, ".exe")
	}
	if base != provider {
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
	return resolvedExecutable, nil
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
