package launch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	tmuxProfileVersion      = 1
	tmuxProfileVersionRange = ">=3.2 <4.0"
	tmuxNonceEnvironment    = "AMQ_LAUNCH_NONCE"
	tmuxCommandTimeout      = 5 * time.Second
)

// TmuxBackend manages one deterministic tmux session per AMQ project/session.
// The socket namespace is stable across tmux server restarts; resource IDs are
// still live tmux IDs and are never reconstructed from display names.
type TmuxBackend struct {
	binary     string
	socketName string
	run        func(context.Context, ...string) (string, error)
	focus      func(context.Context, string) error
	hostname   func() (string, error)
	getenv     func(string) string
	uid        func() string
}

func NewTmuxBackend(binary string) *TmuxBackend {
	if strings.TrimSpace(binary) == "" {
		binary = "tmux"
	}
	b := &TmuxBackend{
		binary: binary, hostname: os.Hostname, getenv: os.Getenv, uid: currentUserID,
	}
	b.run = b.runCommand
	b.focus = b.focusResource
	return b
}

func TmuxProfile() Profile {
	return Profile{
		Backend: LauncherTMux, Platform: runtime.GOOS,
		VersionRange: tmuxProfileVersionRange, Version: tmuxProfileVersion,
		Capabilities: []Capability{CapCreate, CapInspect, CapClose, CapFocus, CapReclaim},
	}
}

func (b *TmuxBackend) Detect() DetectResult {
	profile := TmuxProfile()
	result := DetectResult{Profile: profile}
	_, err := exec.LookPath(b.binary)
	if err != nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	out, err := b.run(ctx, "-V")
	if err != nil || !supportedTmuxVersion(out) {
		return result
	}
	host, err := b.hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return result
	}
	result.Available = true
	result.HostIdentity = host
	result.InstanceIdentity = "tmux-socket:" + b.socketPath()
	result.Effective = append([]Capability(nil), profile.Capabilities...)
	return result
}

func (b *TmuxBackend) Create(req CreateRequest) (CreateResult, error) {
	if err := b.validateCreate(req); err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	amqPath := req.AMQPath
	if strings.TrimSpace(amqPath) == "" {
		amqPath = "amq"
	}
	resolvedAMQ, err := exec.LookPath(amqPath)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("resolve amq executable: %w", err)}
	}
	if resolvedAMQ, err = filepath.EvalSymlinks(resolvedAMQ); err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("resolve amq executable identity: %w", err)}
	}
	req.AMQPath = resolvedAMQ
	detect := b.Detect()
	if !detect.Available {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("tmux %s is unavailable", tmuxProfileVersionRange)}
	}
	name, err := tmuxSessionName(req.ProjectRoot, req.Session)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	present, err := b.hasSession(ctx, name)
	if err != nil {
		return CreateResult{}, err
	}
	if present {
		return CreateResult{}, fmt.Errorf("tmux session %q already exists; journal recovery must classify it", name)
	}

	first := req.Plan.Agents[0]
	firstLine := b.agentCommand(req, first)
	out, err := b.run(ctx, b.args("new-session", "-d", "-P", "-F", "#{session_id}\t#{window_id}",
		"-s", name, "-n", first.Handle, "-e", tmuxNonceEnvironment+"="+first.LaunchNonce, firstLine)...)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create tmux session: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		return CreateResult{}, fmt.Errorf("tmux create returned invalid resource identities %q", strings.TrimSpace(out))
	}
	sessionID := fields[0]
	if err := b.markWindow(ctx, fields[1], first.Handle); err != nil {
		return CreateResult{}, err
	}
	for _, agent := range req.Plan.Agents[1:] {
		windowID, createErr := b.run(ctx, b.args("new-window", "-d", "-P", "-F", "#{window_id}",
			"-t", "="+name, "-n", agent.Handle, b.agentCommand(req, agent))...)
		if createErr != nil {
			return CreateResult{}, fmt.Errorf("create tmux window for %s: %w", agent.Handle, createErr)
		}
		windowID = strings.TrimSpace(windowID)
		if windowID == "" {
			return CreateResult{}, fmt.Errorf("tmux returned no window identity for %s", agent.Handle)
		}
		if err := b.markWindow(ctx, windowID, agent.Handle); err != nil {
			return CreateResult{}, err
		}
	}
	resources, err := b.liveResources(ctx, name)
	if err != nil {
		return CreateResult{}, fmt.Errorf("revalidate created tmux session: %w", err)
	}
	if issues := tmuxInventoryIssues(resources, req.Plan); len(issues) != 0 {
		return CreateResult{}, fmt.Errorf("created tmux inventory differs from plan: %s", strings.Join(issues, ","))
	}
	if resources[0].OpaqueID != tmuxSessionResource(sessionID, name) {
		return CreateResult{}, fmt.Errorf("created tmux session identity changed before binding")
	}
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherTMux, HostIdentity: detect.HostIdentity,
		InstanceIdentity: detect.InstanceIdentity, Profile: detect.Profile.Identity(),
		LaunchNonce: first.LaunchNonce,
		Resources:   ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources},
	}
	if err := binding.Validate(); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Outcome: OutcomeCreated, Profile: binding.Profile, Binding: binding}, nil
}

func (b *TmuxBackend) Inspect(req InspectRequest) (InspectResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	state, err := b.inspect(ctx, req.Binding)
	if err != nil {
		return InspectResult{Status: InspectUnknown, Evidence: err.Error(), ActionRequired: true}, nil
	}
	return state, nil
}

func (b *TmuxBackend) Close(req CloseRequest) (CloseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	inspection, err := b.inspect(ctx, req.Binding)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if inspection.Status == InspectAbsent {
		return CloseResult{Outcome: OutcomeClosed, Reason: "tmux session already absent"}, nil
	}
	if inspection.Status != InspectPresent {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: inspection.Evidence}, nil
	}
	sessionID, _, err := parseTmuxSessionResource(req.Binding)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if _, err := b.run(ctx, b.args("kill-session", "-t", sessionID)...); err != nil {
		return CloseResult{}, fmt.Errorf("close tmux session: %w", err)
	}
	return CloseResult{Outcome: OutcomeClosed, Reason: "tmux session closed"}, nil
}

func (b *TmuxBackend) Focus(req FocusRequest) (FocusResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	inspection, err := b.inspect(ctx, req.Binding)
	if err != nil || inspection.Status != InspectPresent {
		if err != nil {
			return FocusResult{}, err
		}
		return FocusResult{Outcome: OutcomeActionRequired, Reason: inspection.Evidence}, nil
	}
	sessionID, _, err := parseTmuxSessionResource(req.Binding)
	if err != nil {
		return FocusResult{}, err
	}
	if err := b.focus(ctx, sessionID); err != nil {
		return FocusResult{}, err
	}
	return FocusResult{Outcome: OutcomeAttached}, nil
}

func (b *TmuxBackend) focusResource(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(b.getenv("TMUX")) != "" {
		_, err := b.run(ctx, b.args("switch-client", "-t", sessionID)...)
		return err
	}
	cmd := exec.Command(b.binary, b.args("attach-session", "-t", sessionID)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (b *TmuxBackend) Reclaim(req ReclaimRequest) (ReclaimResult, error) {
	parent := req.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, tmuxCommandTimeout)
	defer cancel()
	if req.Root == nil {
		return ReclaimResult{}, fmt.Errorf("missing pinned session root")
	}
	rootIdentity, err := canonicalIdentity(req.Root.Base())
	if err != nil || rootIdentity != req.Journal.RootIdentity {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "journal session root does not match the pinned root"}, nil
	}
	if req.Journal.RootPhysical != "" {
		physical, physicalErr := fsq.StableTreeIdentityInfo(req.Root.FileInfo())
		if physicalErr != nil || physical != req.Journal.RootPhysical {
			return ReclaimResult{Status: ReclaimForeign, Evidence: "journal session root identity changed"}, nil
		}
	}
	name, err := tmuxSessionName(req.Journal.ProjectIdentity, req.Journal.Session)
	if err != nil {
		return ReclaimResult{}, err
	}
	present, err := b.hasSession(ctx, name)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	if !present {
		return ReclaimResult{Status: ReclaimAbsent, Evidence: "deterministic tmux session is absent"}, nil
	}
	nonce, err := b.sessionNonce(ctx, name)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	if nonce != req.Journal.LaunchNonce {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "tmux session nonce does not match the journal"}, nil
	}
	resources, err := b.liveResources(ctx, name)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	issues := tmuxInventoryIssues(resources, req.Journal.Plan)
	if len(issues) != 0 {
		return ReclaimResult{
			Status: ReclaimIncomplete, Evidence: "tmux session inventory differs from plan: " + strings.Join(issues, ","),
			Resources: resources,
		}, nil
	}
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherTMux, HostIdentity: req.Journal.HostIdentity,
		InstanceIdentity: req.Journal.InstanceIdentity, Profile: req.Journal.Profile,
		LaunchNonce: req.Journal.LaunchNonce,
		Resources:   ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources},
	}
	host, hostErr := b.hostname()
	instance := "tmux-socket:" + b.socketPath()
	if hostErr != nil || host != binding.HostIdentity || instance != binding.InstanceIdentity || TmuxProfile().Identity() != binding.Profile {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "tmux runtime identity changed", Resources: resources}, nil
	}
	return ReclaimResult{Status: ReclaimAdoptable, Evidence: "tmux session and nonce match the complete journal plan", Resources: resources, Binding: binding}, nil
}

func (b *TmuxBackend) validateCreate(req CreateRequest) error {
	if strings.TrimSpace(req.ProjectRoot) == "" || strings.TrimSpace(req.Session) == "" || req.Root == nil {
		return fmt.Errorf("project root, session, and pinned root are required")
	}
	if err := req.Plan.Validate(); err != nil {
		return err
	}
	if _, err := canonicalIdentity(req.ProjectRoot); err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	handles := make([]string, len(req.Plan.Agents))
	nonce := req.Plan.Agents[0].LaunchNonce
	if !validUUID(nonce) {
		return fmt.Errorf("launch nonce must be a UUID")
	}
	for i, agent := range req.Plan.Agents {
		handles[i] = agent.Handle
		if agent.LaunchNonce != nonce {
			return fmt.Errorf("agent %q has a different launch nonce", agent.Handle)
		}
	}
	if err := fsq.ValidateExistingMailboxLayout(req.Root, handles...); err != nil {
		return err
	}
	return nil
}

func (b *TmuxBackend) inspect(ctx context.Context, binding BindingRecord) (InspectResult, error) {
	if err := binding.Validate(); err != nil {
		return InspectResult{}, err
	}
	if err := b.validateBindingContext(binding); err != nil {
		return InspectResult{}, err
	}
	sessionID, name, err := parseTmuxSessionResource(binding)
	if err != nil {
		return InspectResult{}, err
	}
	present, err := b.hasSession(ctx, name)
	if err != nil {
		return InspectResult{}, err
	}
	if !present {
		return InspectResult{Status: InspectAbsent, Evidence: "tmux session is absent"}, nil
	}
	nonce, err := b.sessionNonce(ctx, name)
	if err != nil {
		return InspectResult{}, err
	}
	if nonce != binding.LaunchNonce {
		return InspectResult{Status: InspectUnknown, Evidence: "tmux session nonce differs from binding", ActionRequired: true}, nil
	}
	resources, err := b.liveResources(ctx, name)
	if err != nil {
		return InspectResult{}, err
	}
	if len(resources) != len(binding.Resources.Resources) {
		return InspectResult{Status: InspectUnknown, Evidence: "tmux resource inventory differs from binding", ActionRequired: true}, nil
	}
	live := make(map[string]string, len(resources))
	for _, resource := range resources {
		live[resource.OpaqueID] = resource.Agent
	}
	for _, expected := range binding.Resources.Resources {
		if live[expected.OpaqueID] != expected.Agent {
			return InspectResult{Status: InspectUnknown, Evidence: "tmux resource identity differs from binding", ActionRequired: true}, nil
		}
	}
	if resources[0].OpaqueID != tmuxSessionResource(sessionID, name) {
		return InspectResult{Status: InspectUnknown, Evidence: "tmux session identity differs from binding", ActionRequired: true}, nil
	}
	return InspectResult{Status: InspectPresent, Evidence: "tmux session nonce and resource inventory match"}, nil
}

func (b *TmuxBackend) validateBindingContext(binding BindingRecord) error {
	host, err := b.hostname()
	if err != nil {
		return fmt.Errorf("resolve tmux host identity: %w", err)
	}
	if binding.Backend != LauncherTMux || binding.Profile != TmuxProfile().Identity() ||
		binding.HostIdentity != host || binding.InstanceIdentity != "tmux-socket:"+b.socketPath() {
		return fmt.Errorf("tmux binding belongs to a different backend context")
	}
	return nil
}

func (b *TmuxBackend) liveResources(ctx context.Context, name string) ([]ResourceIdentity, error) {
	out, err := b.run(ctx, b.args("list-windows", "-t", "="+name, "-F", "#{session_id}\t#{session_name}\t#{window_id}\t#{@amq_agent}")...)
	if err != nil {
		return nil, fmt.Errorf("inspect tmux windows: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("tmux session has no windows")
	}
	resources := make([]ResourceIdentity, 0, len(lines)+1)
	var sessionID string
	for i, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || fields[0] == "" || fields[1] != name || fields[2] == "" || fields[3] == "" {
			return nil, fmt.Errorf("invalid tmux resource inventory line %q", line)
		}
		if i == 0 {
			sessionID = fields[0]
			resources = append(resources, ResourceIdentity{OpaqueID: tmuxSessionResource(sessionID, name)})
		} else if fields[0] != sessionID {
			return nil, fmt.Errorf("tmux inventory crossed session identities")
		}
		resources = append(resources, ResourceIdentity{OpaqueID: tmuxWindowResource(fields[2]), Agent: fields[3]})
	}
	return resources, nil
}

func (b *TmuxBackend) hasSession(ctx context.Context, name string) (bool, error) {
	_, err := b.run(ctx, b.args("has-session", "-t", "="+name)...)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func (b *TmuxBackend) sessionNonce(ctx context.Context, name string) (string, error) {
	out, err := b.run(ctx, b.args("show-environment", "-t", "="+name, tmuxNonceEnvironment)...)
	if err != nil {
		return "", fmt.Errorf("read tmux launch nonce: %w", err)
	}
	key, value, ok := strings.Cut(strings.TrimSpace(out), "=")
	if !ok || key != tmuxNonceEnvironment || value == "" {
		return "", fmt.Errorf("tmux launch nonce marker is missing")
	}
	return value, nil
}

func (b *TmuxBackend) markWindow(ctx context.Context, windowID, handle string) error {
	if _, err := b.run(ctx, b.args("set-option", "-w", "-t", windowID, "@amq_agent", handle)...); err != nil {
		return fmt.Errorf("mark tmux window for %s: %w", handle, err)
	}
	return nil
}

func (b *TmuxBackend) agentCommand(req CreateRequest, agent AgentPlan) string {
	amq := req.AMQPath
	if strings.TrimSpace(amq) == "" {
		amq = "amq"
	}
	env := cloneEnv(agent.EnvOverlay)
	if env == nil {
		env = make(map[string]string, 1)
	}
	env[InternalLaunchNonceEnv] = agent.LaunchNonce
	return commandLine(agent.Cwd, env, coopExecArgv(amq, req.Session, agent.Handle, agent.Argv))
}

func (b *TmuxBackend) args(args ...string) []string {
	if b.socketName == "" || b.socketName == "default" {
		return args
	}
	return append([]string{"-L", b.socketName}, args...)
}

func (b *TmuxBackend) runCommand(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, b.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (b *TmuxBackend) socketPath() string {
	if active := strings.TrimSpace(b.getenv("TMUX")); b.socketName == "" && active != "" {
		if path, _, ok := strings.Cut(active, ","); ok && filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
	}
	base := strings.TrimSpace(b.getenv("TMUX_TMPDIR"))
	if base == "" {
		base = "/tmp"
	}
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	name := b.socketName
	if name == "" {
		name = "default"
	}
	return filepath.Join(base, "tmux-"+b.uid(), name)
}

func currentUserID() string {
	current, err := user.Current()
	if err != nil || strings.TrimSpace(current.Uid) == "" {
		return "unknown"
	}
	return current.Uid
}

func supportedTmuxVersion(raw string) bool {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) < 2 || fields[0] != "tmux" {
		return false
	}
	version := strings.TrimRightFunc(fields[1], func(r rune) bool { return r < '0' || r > '9' })
	parts := strings.SplitN(version, ".", 2)
	if len(parts) != 2 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	return errMajor == nil && errMinor == nil && major == 3 && minor >= 2
}

func tmuxSessionName(projectRoot, session string) (string, error) {
	canonical, err := canonicalIdentity(projectRoot)
	if err != nil {
		return "", err
	}
	slug := tmuxSlug(filepath.Base(canonical))
	sessionSlug := tmuxSlug(session)
	projectSum := sha256.Sum256([]byte(canonical))
	sessionSum := sha256.Sum256([]byte(session))
	return fmt.Sprintf("amq-%s-%s-%s-%s",
		truncateTMuxPart(slug, 30), hex.EncodeToString(projectSum[:5]),
		truncateTMuxPart(sessionSlug, 24), hex.EncodeToString(sessionSum[:4])), nil
}

func tmuxSlug(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		} else if out.Len() != 0 && !strings.HasSuffix(out.String(), "-") {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func truncateTMuxPart(value string, max int) string {
	if value == "" {
		return "session"
	}
	if len(value) > max {
		return strings.TrimRight(value[:max], "-")
	}
	return value
}

func tmuxSessionResource(id, name string) string { return "tmux:v1:session:" + id + ":" + name }
func tmuxWindowResource(id string) string        { return "tmux:v1:window:" + id }

func parseTmuxSessionResource(binding BindingRecord) (string, string, error) {
	for _, resource := range binding.Resources.Resources {
		if strings.HasPrefix(resource.OpaqueID, "tmux:v1:session:") && resource.Agent == "" {
			parts := strings.SplitN(strings.TrimPrefix(resource.OpaqueID, "tmux:v1:session:"), ":", 2)
			if len(parts) == 2 && strings.HasPrefix(parts[0], "$") && parts[1] != "" {
				return parts[0], parts[1], nil
			}
		}
	}
	return "", "", fmt.Errorf("binding has no valid tmux session identity")
}

func tmuxInventoryIssues(resources []ResourceIdentity, plan Plan) []string {
	expected := make(map[string]bool, len(plan.Agents))
	for _, agent := range plan.Agents {
		expected[agent.Handle] = true
	}
	live := make(map[string]int, len(resources))
	issues := make([]string, 0)
	for _, resource := range resources {
		if resource.Agent != "" {
			live[resource.Agent]++
			if !expected[resource.Agent] {
				issues = append(issues, "unexpected:"+resource.Agent)
			}
		}
	}
	for _, agent := range plan.Agents {
		switch live[agent.Handle] {
		case 0:
			issues = append(issues, "missing:"+agent.Handle)
		case 1:
		default:
			issues = append(issues, "duplicate:"+agent.Handle)
		}
	}
	if len(resources) != len(plan.Agents)+1 {
		issues = append(issues, fmt.Sprintf("resource_count:%d", len(resources)))
	}
	return issues
}
