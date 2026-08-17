package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	cmuxProfileVersion      = 1
	cmuxProfileVersionRange = ">=0.64.3 <1.0"
	cmuxCommandTimeout      = 5 * time.Second
	cmuxHealthTimeout       = 15 * time.Second
	cmuxHealthPoll          = 100 * time.Millisecond
	cmuxBundleCLI           = "/Applications/cmux.app/Contents/Resources/bin/cmux"
	cmuxWorkspacePrefix     = "cmux:v1:workspace:"
	cmuxWindowPrefix        = "cmux:v1:window:"
	cmuxSurfacePrefix       = "cmux:v1:surface:"
	cmuxInstancePrefix      = "cmux-socket:"
)

// CmuxBackend manages one cmux workspace per AMQ project/session generation.
type CmuxBackend struct {
	binary        string
	run           func(context.Context, ...string) (string, error)
	hostname      func() (string, error)
	getenv        func(string) string
	lookPath      func(string) (string, error)
	userHomeDir   func() (string, error)
	isExecutable  func(string) bool
	sleep         func(context.Context, time.Duration) error
	healthTimeout time.Duration
	healthPoll    time.Duration
}

func NewCmuxBackend(binary string) *CmuxBackend {
	b := &CmuxBackend{
		binary: strings.TrimSpace(binary), hostname: os.Hostname, getenv: os.Getenv,
		lookPath: exec.LookPath, userHomeDir: os.UserHomeDir, isExecutable: cmuxExecutable,
		healthTimeout: cmuxHealthTimeout, healthPoll: cmuxHealthPoll,
	}
	b.run = b.runCommand
	return b
}

func CmuxProfile() Profile {
	return Profile{
		Backend: LauncherCMux, Platform: "darwin",
		VersionRange: cmuxProfileVersionRange, Version: cmuxProfileVersion,
		Capabilities: []Capability{CapCreate, CapInspect, CapClose, CapFocus, CapReclaim},
	}
}

func (b *CmuxBackend) Detect() DetectResult {
	profile := CmuxProfile()
	result := DetectResult{Profile: profile}
	if _, err := b.resolveExecutable(); err != nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmuxCommandTimeout)
	defer cancel()
	if _, err := b.run(ctx, "ping"); err != nil {
		return result
	}
	raw, err := b.run(ctx, "--json", "capabilities")
	if err != nil {
		return result
	}
	caps, err := parseCmuxCapabilities(raw)
	if err != nil {
		return result
	}
	host, err := b.hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return result
	}
	socket, err := canonicalCmuxSocket(caps.SocketPath)
	if err != nil {
		return result
	}
	result.HostIdentity = host
	result.InstanceIdentity = cmuxInstancePrefix + socket
	if !supportedCmuxVersion(caps.Version) {
		reason := fmt.Sprintf("cmux version %q is outside %s", caps.Version, cmuxProfileVersionRange)
		for _, cap := range profile.Capabilities {
			result.Degradations = append(result.Degradations, Degradation{Capability: cap, Reason: reason})
		}
		return result
	}
	result.Available = true
	result.Effective = append([]Capability(nil), profile.Capabilities...)
	return result
}

func (b *CmuxBackend) Create(req CreateRequest) (CreateResult, error) {
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
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("cmux %s is unavailable", cmuxProfileVersionRange)}
	}
	nonce := req.Plan.Agents[0].LaunchNonce
	name, err := cmuxWorkspaceName(req.ProjectRoot, req.Session, nonce)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	ctx := context.Background()
	existing, err := b.workspaceByName(ctx, name)
	if err != nil {
		return CreateResult{}, err
	}
	if existing != "" {
		return CreateResult{}, fmt.Errorf("cmux workspace %q already exists; journal recovery must classify it", name)
	}

	createRaw, err := b.runJSON(ctx, "new-workspace", "--name", name, "--cwd", req.ProjectRoot, "--focus", "true")
	if err != nil {
		return CreateResult{}, fmt.Errorf("create cmux workspace: %w", err)
	}
	workspaceID, windowID, err := parseCmuxCreatedWorkspace(createRaw)
	if err != nil {
		return CreateResult{}, err
	}
	firstSurfaces, err := b.workspaceSurfaces(ctx, workspaceID)
	if err != nil {
		return CreateResult{}, err
	}
	if len(firstSurfaces) != 1 {
		return CreateResult{}, fmt.Errorf("cmux workspace created %d surfaces, want 1", len(firstSurfaces))
	}
	surfaceIDs := []string{firstSurfaces[0]}
	for _, agent := range req.Plan.Agents[1:] {
		splitRaw, splitErr := b.runJSON(ctx, "new-split", "right", "--workspace", workspaceID, "--surface", surfaceIDs[len(surfaceIDs)-1], "--focus", "true")
		if splitErr != nil {
			return CreateResult{}, fmt.Errorf("create cmux split for %s: %w", agent.Handle, splitErr)
		}
		surfaceID, splitErr := parseCmuxID(splitRaw)
		if splitErr != nil {
			return CreateResult{}, fmt.Errorf("cmux split for %s: %w", agent.Handle, splitErr)
		}
		surfaceIDs = append(surfaceIDs, surfaceID)
	}
	if err := b.waitHealthy(ctx, workspaceID, surfaceIDs); err != nil {
		return CreateResult{}, err
	}
	for i, agent := range req.Plan.Agents {
		line := b.agentCommand(req, agent)
		if _, err := b.run(ctx, "send", "--surface", surfaceIDs[i], "--", line); err != nil {
			return CreateResult{}, fmt.Errorf("send cmux command for %s: %w", agent.Handle, err)
		}
		if _, err := b.run(ctx, "send-key", "--surface", surfaceIDs[i], "enter"); err != nil {
			return CreateResult{}, fmt.Errorf("submit cmux command for %s: %w", agent.Handle, err)
		}
	}
	resources := cmuxBindingResources(workspaceID, windowID, nonce, req.Plan, surfaceIDs)
	if issues := cmuxInventoryIssues(resources, req.Plan); len(issues) != 0 {
		return CreateResult{}, fmt.Errorf("created cmux inventory differs from plan: %s", strings.Join(issues, ","))
	}
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherCMux, HostIdentity: detect.HostIdentity,
		InstanceIdentity: detect.InstanceIdentity, Profile: detect.Profile.Identity(),
		LaunchNonce: nonce,
		Resources:   ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources},
	}
	if err := binding.Validate(); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Outcome: OutcomeCreated, Profile: binding.Profile, Binding: binding}, nil
}

func (b *CmuxBackend) Inspect(req InspectRequest) (InspectResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmuxCommandTimeout)
	defer cancel()
	state, err := b.inspect(ctx, req.Binding)
	if err != nil {
		return InspectResult{Status: InspectUnknown, Evidence: err.Error(), ActionRequired: true}, nil
	}
	return state, nil
}

func (b *CmuxBackend) Close(req CloseRequest) (CloseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmuxCommandTimeout)
	defer cancel()
	inspection, err := b.inspect(ctx, req.Binding)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if inspection.Status == InspectAbsent {
		return CloseResult{Outcome: OutcomeClosed, Reason: "cmux workspace already absent"}, nil
	}
	if inspection.Status != InspectPresent {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: inspection.Evidence}, nil
	}
	workspaceID, _, err := parseCmuxWorkspaceResource(req.Binding)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if _, err := b.run(ctx, "close-workspace", "--workspace", workspaceID); err != nil {
		return CloseResult{}, fmt.Errorf("close cmux workspace: %w", err)
	}
	return CloseResult{Outcome: OutcomeClosed, Reason: "cmux workspace closed"}, nil
}

func (b *CmuxBackend) Focus(req FocusRequest) (FocusResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmuxCommandTimeout)
	defer cancel()
	inspection, err := b.inspect(ctx, req.Binding)
	if err != nil || inspection.Status != InspectPresent {
		if err != nil {
			return FocusResult{}, err
		}
		return FocusResult{Outcome: OutcomeActionRequired, Reason: inspection.Evidence}, nil
	}
	workspaceID, _, err := parseCmuxWorkspaceResource(req.Binding)
	if err != nil {
		return FocusResult{}, err
	}
	if _, err := b.run(ctx, "select-workspace", "--workspace", workspaceID); err != nil {
		return FocusResult{}, err
	}
	if windowID := cmuxWindowID(req.Binding); windowID != "" {
		if _, err := b.run(ctx, "focus-window", "--window", windowID); err != nil {
			return FocusResult{}, err
		}
	}
	return FocusResult{Outcome: OutcomeAttached}, nil
}

func (b *CmuxBackend) Reclaim(req ReclaimRequest) (ReclaimResult, error) {
	parent := req.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, cmuxCommandTimeout)
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
	name, err := cmuxWorkspaceName(req.Journal.ProjectIdentity, req.Journal.Session, req.Journal.LaunchNonce)
	if err != nil {
		return ReclaimResult{}, err
	}
	workspaceID, err := b.workspaceByName(ctx, name)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	if workspaceID == "" {
		return ReclaimResult{Status: ReclaimAbsent, Evidence: "deterministic cmux workspace is absent"}, nil
	}
	surfaceIDs, err := b.workspaceSurfaces(ctx, workspaceID)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	resources := cmuxBindingResources(workspaceID, "", req.Journal.LaunchNonce, req.Journal.Plan, surfaceIDs)
	issues := cmuxInventoryIssues(resources, req.Journal.Plan)
	if len(issues) != 0 {
		return ReclaimResult{
			Status: ReclaimIncomplete, Evidence: "cmux workspace inventory differs from plan: " + strings.Join(issues, ","),
			Resources: resources,
		}, nil
	}
	detect := b.Detect()
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherCMux, HostIdentity: req.Journal.HostIdentity,
		InstanceIdentity: req.Journal.InstanceIdentity, Profile: req.Journal.Profile,
		LaunchNonce: req.Journal.LaunchNonce,
		Resources:   ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources},
	}
	if !detect.Available || detect.HostIdentity != binding.HostIdentity ||
		detect.InstanceIdentity != binding.InstanceIdentity || CmuxProfile().Identity() != binding.Profile {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "cmux runtime identity changed", Resources: resources}, nil
	}
	return ReclaimResult{Status: ReclaimAdoptable, Evidence: "cmux workspace and nonce match the complete journal plan", Resources: resources, Binding: binding}, nil
}

func (b *CmuxBackend) validateCreate(req CreateRequest) error {
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

func (b *CmuxBackend) inspect(ctx context.Context, binding BindingRecord) (InspectResult, error) {
	if err := binding.Validate(); err != nil {
		return InspectResult{}, err
	}
	if err := b.validateBindingContext(binding); err != nil {
		return InspectResult{}, err
	}
	workspaceID, nonce, err := parseCmuxWorkspaceResource(binding)
	if err != nil {
		return InspectResult{}, err
	}
	if nonce != binding.LaunchNonce {
		return InspectResult{Status: InspectUnknown, Evidence: "cmux workspace nonce differs from binding", ActionRequired: true}, nil
	}
	listed, err := b.listWorkspaces(ctx)
	if err != nil {
		return InspectResult{}, err
	}
	if !cmuxContainsID(listed, workspaceID) {
		return InspectResult{Status: InspectAbsent, Evidence: "cmux workspace is absent"}, nil
	}
	live, err := b.workspaceSurfaces(ctx, workspaceID)
	if err != nil {
		return InspectResult{}, err
	}
	want := map[string]string{}
	for _, resource := range binding.Resources.Resources {
		if resource.Agent != "" {
			id, parseErr := parseCmuxSurfaceResource(resource.OpaqueID)
			if parseErr != nil {
				return InspectResult{}, parseErr
			}
			want[id] = resource.Agent
		}
	}
	if len(live) != len(want) {
		return InspectResult{Status: InspectUnknown, Evidence: "cmux resource inventory differs from binding", ActionRequired: true}, nil
	}
	liveSet := map[string]bool{}
	for _, id := range live {
		liveSet[id] = true
	}
	for id := range want {
		if !liveSet[id] {
			return InspectResult{Status: InspectUnknown, Evidence: "cmux resource identity differs from binding", ActionRequired: true}, nil
		}
	}
	return InspectResult{Status: InspectPresent, Evidence: "cmux workspace and surface inventory match"}, nil
}

func (b *CmuxBackend) validateBindingContext(binding BindingRecord) error {
	host, err := b.hostname()
	if err != nil {
		return fmt.Errorf("resolve cmux host identity: %w", err)
	}
	if binding.Backend != LauncherCMux || binding.Profile != CmuxProfile().Identity() ||
		binding.HostIdentity != host {
		return fmt.Errorf("cmux binding belongs to a different backend context")
	}
	detect := b.Detect()
	if detect.InstanceIdentity == "" {
		return fmt.Errorf("cmux socket is unreachable")
	}
	if binding.InstanceIdentity != detect.InstanceIdentity {
		return fmt.Errorf("cmux binding belongs to a different backend context")
	}
	return nil
}

func (b *CmuxBackend) waitHealthy(parent context.Context, workspaceID string, surfaceIDs []string) error {
	timeout := b.healthTimeout
	if timeout <= 0 {
		timeout = cmuxHealthTimeout
	}
	poll := b.healthPoll
	if poll <= 0 {
		poll = cmuxHealthPoll
	}
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(parent, cmuxCommandTimeout)
		raw, err := b.runJSON(ctx, "surface-health", "--workspace", workspaceID)
		cancel()
		if err == nil {
			if ready, readyErr := cmuxSurfacesHealthy(raw, surfaceIDs); readyErr != nil {
				return readyErr
			} else if ready {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("cmux surface readiness timed out: %w", err)
			}
			return fmt.Errorf("cmux surface readiness timed out before sending commands")
		}
		if err := b.doSleep(parent, poll); err != nil {
			return err
		}
	}
}

func (b *CmuxBackend) workspaceSurfaces(ctx context.Context, workspaceID string) ([]string, error) {
	raw, err := b.runJSON(ctx, "list-panes", "--workspace", workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list cmux panes: %w", err)
	}
	panes, err := parseCmuxIDList(raw, "panes")
	if err != nil {
		return nil, err
	}
	if len(panes) == 0 {
		return nil, fmt.Errorf("cmux workspace has no panes")
	}
	var surfaces []string
	for _, pane := range panes {
		paneRaw, paneErr := b.runJSON(ctx, "list-pane-surfaces", "--workspace", workspaceID, "--pane", pane)
		if paneErr != nil {
			return nil, fmt.Errorf("list cmux pane surfaces: %w", paneErr)
		}
		ids, paneErr := parseCmuxIDList(paneRaw, "surfaces")
		if paneErr != nil {
			return nil, paneErr
		}
		surfaces = append(surfaces, ids...)
	}
	return surfaces, nil
}

func (b *CmuxBackend) workspaceByName(ctx context.Context, name string) (string, error) {
	workspaces, err := b.listWorkspaceRecords(ctx)
	if err != nil {
		return "", err
	}
	for _, workspace := range workspaces {
		if workspace.Title == name {
			return strings.ToLower(workspace.ID), nil
		}
	}
	return "", nil
}

func (b *CmuxBackend) listWorkspaces(ctx context.Context) ([]string, error) {
	records, err := b.listWorkspaceRecords(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(records))
	for _, workspace := range records {
		ids = append(ids, strings.ToLower(workspace.ID))
	}
	return ids, nil
}

func (b *CmuxBackend) listWorkspaceRecords(ctx context.Context) ([]cmuxListedWorkspace, error) {
	raw, err := b.runJSON(ctx, "list-workspaces")
	if err != nil {
		return nil, fmt.Errorf("list cmux workspaces: %w", err)
	}
	return parseCmuxWorkspaceList(raw)
}

func (b *CmuxBackend) agentCommand(req CreateRequest, agent AgentPlan) string {
	amq := req.AMQPath
	if strings.TrimSpace(amq) == "" {
		amq = "amq"
	}
	env := cloneEnv(agent.EnvOverlay)
	if env == nil {
		env = make(map[string]string, 1)
	}
	env[InternalLaunchNonceEnv] = agent.LaunchNonce
	return commandLine(agent.Cwd, env, coopExecArgv(amq, req.Root.Base(), agent.Handle, agent.Argv, agent.Execution))
}

func (b *CmuxBackend) runJSON(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--id-format", "uuids", "--json"}, args...)
	return b.run(ctx, full...)
}

func (b *CmuxBackend) runCommand(ctx context.Context, args ...string) (string, error) {
	path, err := b.resolveExecutable()
	if err != nil {
		return "", err
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmuxCommandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("cmux %s: %w: %s", strings.Join(redactCmuxArgs(args), " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (b *CmuxBackend) resolveExecutable() (string, error) {
	if strings.TrimSpace(b.binary) != "" {
		return filepath.Clean(b.binary), nil
	}
	if path, err := b.lookPath("cmux"); err == nil {
		return filepath.Clean(path), nil
	}
	candidates := []string{cmuxBundleCLI}
	if home, err := b.userHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		candidates = append(candidates, filepath.Join(home, "Applications", "cmux.app", "Contents", "Resources", "bin", "cmux"))
	}
	for _, candidate := range candidates {
		if b.isExecutable(candidate) {
			return filepath.Clean(candidate), nil
		}
	}
	return "", fmt.Errorf("cmux CLI not found")
}

func (b *CmuxBackend) doSleep(ctx context.Context, delay time.Duration) error {
	if b.sleep != nil {
		return b.sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type cmuxCapabilities struct {
	SocketPath string `json:"socket_path"`
	Version    string `json:"version"`
}

type cmuxListedWorkspace struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type cmuxCreatedWorkspace struct {
	ID       string `json:"id"`
	WindowID string `json:"window_id"`
}

type cmuxHealthSurface struct {
	ID       string `json:"id"`
	InWindow *bool  `json:"in_window"`
}

func parseCmuxCapabilities(raw string) (cmuxCapabilities, error) {
	var caps cmuxCapabilities
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return cmuxCapabilities{}, fmt.Errorf("parse cmux capabilities: %w", err)
	}
	if strings.TrimSpace(caps.SocketPath) == "" || strings.TrimSpace(caps.Version) == "" {
		return cmuxCapabilities{}, fmt.Errorf("cmux capabilities missing socket_path or version")
	}
	return caps, nil
}

func parseCmuxWorkspaceList(raw string) ([]cmuxListedWorkspace, error) {
	var parsed struct {
		Workspaces []cmuxListedWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse cmux workspaces: %w", err)
	}
	if parsed.Workspaces == nil {
		return nil, fmt.Errorf("cmux workspaces list is missing")
	}
	for i, workspace := range parsed.Workspaces {
		id, err := normalizeCmuxUUID(workspace.ID)
		if err != nil {
			return nil, fmt.Errorf("cmux workspaces[%d]: %w", i, err)
		}
		parsed.Workspaces[i].ID = id
	}
	return parsed.Workspaces, nil
}

func parseCmuxCreatedWorkspace(raw string) (string, string, error) {
	var parsed cmuxCreatedWorkspace
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", "", fmt.Errorf("parse cmux new-workspace: %w", err)
	}
	id, err := normalizeCmuxUUID(parsed.ID)
	if err != nil {
		return "", "", fmt.Errorf("cmux new-workspace: %w", err)
	}
	windowID := strings.TrimSpace(parsed.WindowID)
	if windowID != "" {
		windowID, err = normalizeCmuxUUID(windowID)
		if err != nil {
			return "", "", fmt.Errorf("cmux new-workspace window_id: %w", err)
		}
	}
	return id, windowID, nil
}

func parseCmuxID(raw string) (string, error) {
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", fmt.Errorf("parse cmux id: %w", err)
	}
	return normalizeCmuxUUID(parsed.ID)
}

func parseCmuxIDList(raw, field string) ([]string, error) {
	var parsed map[string][]struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse cmux %s: %w", field, err)
	}
	items, ok := parsed[field]
	if !ok || items == nil {
		return nil, fmt.Errorf("cmux %s list is missing", field)
	}
	ids := make([]string, 0, len(items))
	for i, item := range items {
		id, err := normalizeCmuxUUID(item.ID)
		if err != nil {
			return nil, fmt.Errorf("cmux %s[%d]: %w", field, i, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func cmuxSurfacesHealthy(raw string, want []string) (bool, error) {
	var parsed struct {
		Surfaces []cmuxHealthSurface `json:"surfaces"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return false, fmt.Errorf("parse cmux surface-health: %w", err)
	}
	if parsed.Surfaces == nil {
		return false, fmt.Errorf("cmux surface-health list is missing")
	}
	health := map[string]bool{}
	for i, surface := range parsed.Surfaces {
		id, err := normalizeCmuxUUID(surface.ID)
		if err != nil {
			return false, fmt.Errorf("cmux surface-health[%d]: %w", i, err)
		}
		if surface.InWindow == nil {
			return false, fmt.Errorf("cmux surface-health[%d]: in_window is required", i)
		}
		health[id] = *surface.InWindow
	}
	for _, id := range want {
		if !health[id] {
			return false, nil
		}
	}
	return true, nil
}

func normalizeCmuxUUID(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !validUUID(id) {
		return "", fmt.Errorf("id %q is not a UUID", id)
	}
	return id, nil
}

func canonicalCmuxSocket(path string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("cmux socket path must be absolute")
	}
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved, nil
	}
	return cleaned, nil
}

func supportedCmuxVersion(raw string) bool {
	version := strings.TrimSpace(raw)
	if fields := strings.Fields(version); len(fields) > 0 {
		version = strings.TrimPrefix(fields[0], "cmux")
		if version == "" && len(fields) > 1 {
			version = fields[1]
		}
		version = strings.TrimSpace(strings.TrimPrefix(version, "-"))
	}
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	patch := 0
	if len(parts) > 2 {
		patch, _ = strconv.Atoi(strings.TrimRightFunc(parts[2], func(r rune) bool { return r < '0' || r > '9' }))
	}
	if errMajor != nil || errMinor != nil || major != 0 {
		return false
	}
	if minor > 64 {
		return true
	}
	return minor == 64 && patch >= 3
}

func cmuxWorkspaceName(projectRoot, session, nonce string) (string, error) {
	canonical, err := canonicalIdentity(projectRoot)
	if err != nil {
		return "", err
	}
	if !validUUID(nonce) {
		return "", fmt.Errorf("launch nonce must be a UUID")
	}
	return fmt.Sprintf("amq-%s-%s-%s",
		truncateTMuxPart(tmuxSlug(filepath.Base(canonical)), 20),
		truncateTMuxPart(tmuxSlug(session), 16), nonce), nil
}

func cmuxBindingResources(workspaceID, windowID, nonce string, plan Plan, surfaceIDs []string) []ResourceIdentity {
	resources := []ResourceIdentity{{OpaqueID: cmuxWorkspacePrefix + workspaceID + ":" + nonce}}
	if windowID != "" {
		resources = append(resources, ResourceIdentity{OpaqueID: cmuxWindowPrefix + windowID})
	}
	for i, agent := range plan.Agents {
		if i < len(surfaceIDs) {
			resources = append(resources, ResourceIdentity{OpaqueID: cmuxSurfacePrefix + surfaceIDs[i], Agent: agent.Handle})
		}
	}
	return resources
}

func parseCmuxWorkspaceResource(binding BindingRecord) (string, string, error) {
	for _, resource := range binding.Resources.Resources {
		rest, ok := strings.CutPrefix(resource.OpaqueID, cmuxWorkspacePrefix)
		if !ok || resource.Agent != "" {
			continue
		}
		uuid, nonce, found := strings.Cut(rest, ":")
		if !found {
			return "", "", fmt.Errorf("binding has no valid cmux workspace identity")
		}
		id, err := normalizeCmuxUUID(uuid)
		if err != nil || !validUUID(nonce) {
			return "", "", fmt.Errorf("binding has no valid cmux workspace identity")
		}
		return id, nonce, nil
	}
	return "", "", fmt.Errorf("binding has no valid cmux workspace identity")
}

func parseCmuxSurfaceResource(opaqueID string) (string, error) {
	id, ok := strings.CutPrefix(opaqueID, cmuxSurfacePrefix)
	if !ok {
		return "", fmt.Errorf("invalid cmux surface identity %q", opaqueID)
	}
	return normalizeCmuxUUID(id)
}

func cmuxWindowID(binding BindingRecord) string {
	for _, resource := range binding.Resources.Resources {
		id, ok := strings.CutPrefix(resource.OpaqueID, cmuxWindowPrefix)
		if ok && resource.Agent == "" {
			if normalized, err := normalizeCmuxUUID(id); err == nil {
				return normalized
			}
		}
	}
	return ""
}

func cmuxContainsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func cmuxInventoryIssues(resources []ResourceIdentity, plan Plan) []string {
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
	return issues
}

func cmuxExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

func redactCmuxArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i, arg := range out {
		if arg == "--password" && i+1 < len(out) {
			out[i+1] = "<redacted>"
		}
	}
	return out
}

func prependInsideCmuxPreference(prefs []string) []string {
	if strings.TrimSpace(os.Getenv("CMUX_SURFACE_ID")) == "" {
		return prefs
	}
	out := []string{LauncherCMux}
	for _, name := range prefs {
		if name != LauncherCMux {
			out = append(out, name)
		}
	}
	return out
}
