package launch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ACPProvider names the launch adapter for an Agent Client Protocol agent
// served by amq-acp host. The config's adapter field selects it; no
// executable basename does, because the agent's command is the operator's.
const ACPProvider = "acp"

// acpHostCommand is the companion that serves the agent as an AMQ
// participant.
const acpHostCommand = "amq-acp"

// acpNoWakeReason is the audit reason for the seat's disabled wake: the host
// consumes its own inbox, so a doorbell has nothing to wake.
const acpNoWakeReason = "amq-acp host consumes its own inbox"

// ACPAdapter plans `amq-acp host -- <agent argv>`. Launch owns no
// conversation identity for the seat: the host records its ACP session and
// loads it again on every start, so every launch is planned fresh.
type ACPAdapter struct {
	amq   string
	probe commandProbe
}

// NewACPAdapter builds the adapter. amqExecutable is the running amq; the
// amq-acp beside it is preferred over one found through PATH.
func NewACPAdapter(amqExecutable string) *ACPAdapter {
	return &ACPAdapter{amq: amqExecutable, probe: systemCommandProbe{}}
}

func (adapter *ACPAdapter) Name() string      { return ACPProvider }
func (adapter *ACPAdapter) Mode() AdapterMode { return AdapterModeUnsupported }

func (adapter *ACPAdapter) CommittedEnvKeys() []string {
	return committedEnvKeys(commonCommittedEnvRules())
}

// ValidateCommittedConfig admits any agent argv, which the plan trust digest
// binds byte for byte, and the common environment and cwd rules.
func (adapter *ACPAdapter) ValidateCommittedConfig(request CommittedConfigRequest) error {
	return validateCommittedConfig(CommittedConfigRequest{ProjectRoot: request.ProjectRoot, Cwd: request.Cwd, EnvOverlay: request.EnvOverlay}, commonCommittedEnvRules(), nil)
}

func (adapter *ACPAdapter) Capabilities(ctx context.Context) AdapterCapabilities {
	result := AdapterCapabilities{Provider: ACPProvider, Mode: adapter.Mode()}
	host, err := acpHostPath(adapter.amq, adapter.probe)
	if err != nil {
		result.Reason = "executable_not_found"
		return result
	}
	version, err := adapter.probe.Output(ctx, host, "--version")
	if err != nil || strings.TrimSpace(string(version)) == "" {
		result.Reason = "version_probe_failed"
		return result
	}
	result.Available = true
	result.Executable = host
	result.ProviderVersion = strings.TrimSpace(string(version))
	result.Fresh = true
	return result
}

func (adapter *ACPAdapter) PlanFresh(request PlanRequest) (AgentPlan, error) {
	if !validUUID(request.LaunchNonce) {
		return AgentPlan{}, fmt.Errorf("launch nonce must be a UUID")
	}
	if request.ResumePolicy != ResumeFresh && request.ResumePolicy != ResumeDisabled {
		return AgentPlan{}, fmt.Errorf("the ACP host resumes its own session; use resume_policy %q", ResumeFresh)
	}
	if len(request.Command) == 0 {
		return AgentPlan{}, fmt.Errorf("acp seat has no agent command")
	}
	if len(request.BypassArgs) != 0 {
		return AgentPlan{}, fmt.Errorf("acp seat takes no operator bypass arguments")
	}
	if request.InitialInput != nil {
		return AgentPlan{}, fmt.Errorf("initial input is unsupported for an acp seat")
	}
	hostPath, err := acpHostPath(request.AMQExecutable, adapter.probe)
	if err != nil {
		return AgentPlan{}, err
	}
	host, err := resolveExternalExecutable(hostPath, request.ProjectRoot, acpHostCommand, false)
	if err != nil {
		return AgentPlan{}, err
	}
	agent, err := resolveExternalExecutable(request.Command[0], request.ProjectRoot, "acp agent", false)
	if err != nil {
		return AgentPlan{}, err
	}
	if err := validateCommittedEnv(request.EnvOverlay, commonCommittedEnvRules()); err != nil {
		return AgentPlan{}, err
	}
	if err := validatePlanWorkingDirectory(request); err != nil {
		return AgentPlan{}, err
	}
	plan := AgentPlan{
		Handle: request.Handle, Argv: acpHostArgv(host, agent, request.Command[1:]), EnvOverlay: cloneEnv(request.EnvOverlay),
		Cwd: request.Cwd, AdapterMode: AdapterModeUnsupported, ResumePolicy: request.ResumePolicy, LaunchNonce: request.LaunchNonce,
	}
	if err := ValidateAdapterPlan(adapter, plan); err != nil {
		return AgentPlan{}, err
	}
	return plan, nil
}

// acpHostArgv is the one place that builds the seat's command line.
func acpHostArgv(host, agent string, agentArgs []string) []string {
	argv := []string{host, "host", "--", agent}
	return append(argv, agentArgs...)
}

func (adapter *ACPAdapter) PlanResume(ResumeRequest) (AgentPlan, error) {
	return AgentPlan{}, fmt.Errorf("the ACP host resumes its own session; use resume_policy %q", ResumeFresh)
}

func (adapter *ACPAdapter) CaptureIdentity(CaptureRequest) CaptureResult {
	return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonEvidenceSource}
}

// inboxReason marks a seat whose process consumes its own inbox: launch runs
// it with wake disabled and this audit reason.
func (adapter *ACPAdapter) inboxReason() string { return acpNoWakeReason }

// acpHostPath finds amq-acp beside the amq executable, else through PATH.
func acpHostPath(amq string, probe commandProbe) (string, error) {
	name := acpHostCommand
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if strings.TrimSpace(amq) != "" {
		sibling := filepath.Join(filepath.Dir(amq), name)
		if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() {
			return sibling, nil
		}
	}
	path, err := probe.LookPath(acpHostCommand)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable: %w", acpHostCommand, err)
	}
	return path, nil
}
