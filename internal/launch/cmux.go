package launch

import (
	"bytes"
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
	cmuxCreateTimeout       = 30 * time.Second
	cmuxHealthTimeout       = 15 * time.Second
	cmuxHealthPoll          = 100 * time.Millisecond
	cmuxBundleCLI           = "/Applications/cmux.app/Contents/Resources/bin/cmux"
	cmuxWorkspacePrefix     = "cmux:v1:workspace:"
	cmuxWindowPrefix        = "cmux:v1:window:"
	cmuxSurfacePrefix       = "cmux:v1:surface:"
	cmuxInstancePrefix      = "cmux-socket:"
	cmuxProtocolVersion     = 2
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
	createTimeout time.Duration
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
		cmuxUnsupported(&result, "cmux capabilities: "+err.Error())
		return result
	}
	caps, err := parseCmuxCapabilities(raw)
	if err != nil {
		cmuxUnsupported(&result, err.Error())
		return result
	}
	host, err := b.hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		cmuxUnsupported(&result, "cmux host identity is unavailable")
		return result
	}
	socket, err := canonicalCmuxSocket(caps.SocketPath)
	if err != nil {
		cmuxUnsupported(&result, "cmux socket_path: "+err.Error())
		return result
	}
	result.HostIdentity = host
	result.InstanceIdentity = cmuxInstancePrefix + socket
	if !caps.ProtocolIsInt {
		cmuxUnsupported(&result, "cmux capabilities.version must be protocol integer 2")
		return result
	}
	if caps.ProtocolVersion != cmuxProtocolVersion {
		cmuxUnsupported(&result, fmt.Sprintf("cmux protocol version %d is unsupported (want %d)", caps.ProtocolVersion, cmuxProtocolVersion))
		return result
	}
	verRaw, err := b.run(ctx, "version")
	if err != nil {
		cmuxUnsupported(&result, "cmux version: "+err.Error())
		return result
	}
	appVersion, err := parseCmuxCLIVersion(verRaw)
	if err != nil {
		cmuxUnsupported(&result, err.Error())
		return result
	}
	if !supportedCmuxVersion(appVersion) {
		cmuxUnsupported(&result, fmt.Sprintf("cmux version %q is outside %s", appVersion, cmuxProfileVersionRange))
		return result
	}
	result.Available = true
	result.Effective = append([]Capability(nil), profile.Capabilities...)
	return result
}

func cmuxUnsupported(result *DetectResult, reason string) {
	for _, cap := range result.Profile.Capabilities {
		result.Degradations = append(result.Degradations, Degradation{Capability: cap, Reason: reason})
	}
}

func (b *CmuxBackend) Create(req CreateRequest) (CreateResult, error) {
	if req.JoinBinding != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: placementError(PlacementUnsupportedReason)}
	}
	preview, err := resolveCreatePlacement(LauncherCMux, req)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
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
	inspectCtx, inspectCancel := b.inspectCallContext()
	before, err := b.listWorkspaceRecords(inspectCtx)
	inspectCancel()
	if err != nil {
		return CreateResult{}, err
	}
	previousSelected := cmuxSelectedWorkspaceID(before)
	var workspaceID string
	defer func() {
		b.restoreSelectedWorkspace(previousSelected, workspaceID)
	}()
	if countNamedCmuxWorkspaces(before, name) > 0 {
		return CreateResult{}, fmt.Errorf("cmux workspace %q already exists; journal recovery must classify it", name)
	}

	createCtx, createCancel := b.createCallContext()
	createRaw, err := b.runJSON(createCtx, "new-workspace", "--name", name, "--cwd", req.ProjectRoot, "--focus", "false")
	createCancel()
	if err != nil {
		return CreateResult{}, b.reconcileCreateFailure("", false, fmt.Errorf("create cmux workspace: %w", err))
	}
	if err := parseCmuxOKWorkspaceAck(createRaw); err != nil {
		return CreateResult{}, b.reconcileCreateFailure("", false, err)
	}
	resolveCtx, resolveCancel := b.inspectCallContext()
	createdWS, err := b.uniqueWorkspaceByName(resolveCtx, name)
	resolveCancel()
	if err != nil {
		return CreateResult{}, b.reconcileCreateFailure("", false, err)
	}
	workspaceID = createdWS.ID
	windowID := createdWS.WindowID
	surfaceCtx, surfaceCancel := b.inspectCallContext()
	firstSurfaces, err := b.workspaceSurfaces(surfaceCtx, workspaceID)
	surfaceCancel()
	if err != nil {
		return CreateResult{}, b.reconcileCreateFailure(workspaceID, false, err)
	}
	if len(firstSurfaces) != 1 {
		return CreateResult{}, b.reconcileCreateFailure(workspaceID, false, fmt.Errorf("cmux workspace created %d surfaces, want 1", len(firstSurfaces)))
	}
	surfaceIDs := []string{firstSurfaces[0]}
	for i, agent := range req.Plan.Agents[1:] {
		if err := b.staggerPlacement(preview, i+1); err != nil {
			return CreateResult{}, err
		}
		splitCtx, splitCancel := b.createCallContext()
		splitRaw, splitErr := b.runJSON(splitCtx, "new-split", cmuxSplitDirection(preview.Effective.Layout), "--workspace", workspaceID, "--surface", surfaceIDs[len(surfaceIDs)-1], "--focus", "false")
		splitCancel()
		if splitErr != nil {
			return CreateResult{}, b.reconcileCreateFailure(workspaceID, false, fmt.Errorf("create cmux split for %s: %w", agent.Handle, splitErr))
		}
		surfaceID, splitErr := parseCmuxSplitSurface(splitRaw)
		if splitErr != nil {
			return CreateResult{}, b.reconcileCreateFailure(workspaceID, false, fmt.Errorf("cmux split for %s: %w", agent.Handle, splitErr))
		}
		surfaceIDs = append(surfaceIDs, surfaceID)
	}
	if _, err := b.waitHealthy(context.Background(), workspaceID, len(surfaceIDs)); err != nil {
		return CreateResult{}, b.reconcileCreateFailure(workspaceID, false, err)
	}
	for i, agent := range req.Plan.Agents {
		line := b.agentCommand(req, agent)
		sendCtx, sendCancel := b.inspectCallContext()
		_, err := b.run(sendCtx, "send", "--workspace", workspaceID, "--surface", surfaceIDs[i], "--", line)
		sendCancel()
		if err != nil {
			return CreateResult{}, b.reconcileCreateFailure(workspaceID, true, fmt.Errorf("send cmux command for %s: %w", agent.Handle, err))
		}
		enterCtx, enterCancel := b.inspectCallContext()
		_, err = b.run(enterCtx, "send-key", "--workspace", workspaceID, "--surface", surfaceIDs[i], "enter")
		enterCancel()
		if err != nil {
			return CreateResult{}, b.reconcileCreateFailure(workspaceID, true, fmt.Errorf("submit cmux command for %s: %w", agent.Handle, err))
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
		Placement:   preview,
	}
	if err := binding.Validate(); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Outcome: OutcomeCreated, Profile: binding.Profile, Binding: binding}, nil
}

func (b *CmuxBackend) createOpTimeout() time.Duration {
	if b.createTimeout > 0 {
		return b.createTimeout
	}
	return cmuxCreateTimeout
}

func (b *CmuxBackend) createCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), b.createOpTimeout())
}

func (b *CmuxBackend) staggerPlacement(preview PlacementPreview, index int) error {
	delay := placementStaggerDuration(&preview.Effective)
	if delay <= 0 || index == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), delay+time.Second)
	defer cancel()
	return b.doSleep(ctx, delay)
}

func (b *CmuxBackend) inspectCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cmuxCommandTimeout)
}

func (b *CmuxBackend) orphanCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cmuxCreateTimeout)
}

func (b *CmuxBackend) restoreSelectedWorkspace(previousSelected, skipID string) {
	if previousSelected == "" || previousSelected == skipID {
		return
	}
	ctx, cancel := b.inspectCallContext()
	defer cancel()
	records, err := b.listWorkspaceRecords(ctx)
	if err != nil {
		return
	}
	if !cmuxContainsID(cmuxWorkspaceIDs(records), previousSelected) {
		return
	}
	if cmuxSelectedWorkspaceID(records) == previousSelected {
		return
	}
	_, _ = b.run(ctx, "select-workspace", "--workspace", previousSelected)
}

func (b *CmuxBackend) reconcileCreateFailure(knownWorkspaceID string, inputSent bool, cause error) error {
	if inputSent {
		return cause
	}
	if knownWorkspaceID == "" {
		return fmt.Errorf("%w (cmux workspace ownership unknown, never guessed)", cause)
	}
	ctx, cancel := b.orphanCallContext()
	defer cancel()
	records, err := b.listWorkspaceRecords(ctx)
	if err != nil {
		return fmt.Errorf("%w (orphan workspace reconciliation failed: %v)", cause, err)
	}
	id := strings.ToLower(knownWorkspaceID)
	if !cmuxContainsID(cmuxWorkspaceIDs(records), id) {
		return fmt.Errorf("%w (acknowledged cmux workspace %s is absent)", cause, id)
	}
	if _, closeErr := b.run(ctx, "close-workspace", "--workspace", id); closeErr != nil {
		return fmt.Errorf("%w (close orphan %s: %v)", cause, id, closeErr)
	}
	return fmt.Errorf("%w (closed orphan cmux workspace %s)", cause, id)
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
	surfaces := cmuxOwnedSurfaceIDs(req.Binding)
	if len(surfaces) == 0 {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: "cmux binding has no surface identities"}, nil
	}
	workspaceID, _, err := parseCmuxWorkspaceResource(req.Binding)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	for _, surfaceID := range surfaces {
		if _, closeErr := b.run(ctx, "close-surface", "--surface", surfaceID); closeErr != nil && !cmuxTargetMissing(closeErr) {
			return CloseResult{}, fmt.Errorf("close cmux surface %s: %w", surfaceID, closeErr)
		}
	}
	live, liveErr := b.ownedCmuxSurfacesStillLive(ctx, workspaceID, surfaces)
	if liveErr != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: liveErr.Error()}, nil
	}
	if len(live) > 0 {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: "owned cmux surface still alive after close"}, nil
	}
	return CloseResult{Outcome: OutcomeClosed, Reason: "cmux surfaces closed"}, nil
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
	matches, err := b.namedWorkspaces(ctx, name)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	if req.Journal.Phase == JournalIntent {
		if len(matches) == 0 {
			return ReclaimResult{Status: ReclaimAbsent, Evidence: "deterministic cmux workspace is absent"}, nil
		}
		return ReclaimResult{Status: ReclaimForeign, Evidence: "journal intent must not adopt a same-name cmux workspace"}, nil
	}
	if len(matches) == 0 {
		return ReclaimResult{Status: ReclaimAbsent, Evidence: "deterministic cmux workspace is absent"}, nil
	}
	if len(matches) != 1 {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: fmt.Sprintf("ambiguous cmux workspaces named %q", name)}, nil
	}
	workspace := matches[0]
	if workspace.WindowID == "" {
		return ReclaimResult{Status: ReclaimIncomplete, Evidence: "cmux workspace has no window identity"}, nil
	}
	surfaceIDs, err := b.workspaceSurfaces(ctx, workspace.ID)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	resources := cmuxBindingResources(workspace.ID, workspace.WindowID, req.Journal.LaunchNonce, req.Journal.Plan, surfaceIDs)
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
		Placement:   req.Journal.Placement,
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

func (b *CmuxBackend) waitHealthy(parent context.Context, workspaceID string, want int) (bool, error) {
	timeout := b.healthTimeout
	if timeout <= 0 {
		timeout = cmuxHealthTimeout
	}
	poll := b.healthPoll
	if poll <= 0 {
		poll = cmuxHealthPoll
	}
	deadline := time.Now().Add(timeout)
	didSelect := false
	for {
		ctx, cancel := context.WithTimeout(parent, cmuxCommandTimeout)
		raw, err := b.runJSON(ctx, "surface-health", "--workspace", workspaceID)
		if err == nil {
			ready, needSelect, readyErr := cmuxSurfacesHealthy(raw, want)
			if readyErr != nil {
				cancel()
				return didSelect, readyErr
			}
			if ready {
				cancel()
				return didSelect, nil
			}
			if needSelect && !didSelect {
				if _, selErr := b.run(ctx, "select-workspace", "--workspace", workspaceID); selErr != nil {
					cancel()
					return false, selErr
				}
				didSelect = true
			}
		}
		cancel()
		if time.Now().After(deadline) {
			if err != nil {
				return didSelect, fmt.Errorf("cmux surface readiness timed out: %w", err)
			}
			return didSelect, fmt.Errorf("cmux surface readiness timed out before sending commands")
		}
		if err := b.doSleep(parent, poll); err != nil {
			return didSelect, err
		}
	}
}

func (b *CmuxBackend) workspaceSurfaces(ctx context.Context, workspaceID string) ([]string, error) {
	raw, err := b.runJSON(ctx, "list-panes", "--workspace", workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list cmux panes: %w", err)
	}
	panes, err := parseCmuxPaneIDs(raw)
	if err != nil {
		return nil, err
	}
	if len(panes) == 0 {
		return nil, fmt.Errorf("cmux workspace has no panes")
	}
	var terminals []string
	for _, pane := range panes {
		paneRaw, paneErr := b.runJSON(ctx, "list-pane-surfaces", "--workspace", workspaceID, "--pane", pane)
		if paneErr != nil {
			return nil, fmt.Errorf("list cmux pane surfaces: %w", paneErr)
		}
		ids, paneErr := parseCmuxTerminalSurfaceIDs(paneRaw)
		if paneErr != nil {
			return nil, paneErr
		}
		terminals = append(terminals, ids...)
	}
	if len(terminals) == 0 {
		return nil, fmt.Errorf("cmux workspace has no terminal surfaces")
	}
	return terminals, nil
}

func (b *CmuxBackend) uniqueWorkspaceByName(ctx context.Context, name string) (cmuxListedWorkspace, error) {
	matches, err := b.namedWorkspaces(ctx, name)
	if err != nil {
		return cmuxListedWorkspace{}, err
	}
	switch len(matches) {
	case 0:
		return cmuxListedWorkspace{}, fmt.Errorf("no cmux workspace named %q appeared", name)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, workspace := range matches {
			ids = append(ids, strings.ToLower(workspace.ID))
		}
		return cmuxListedWorkspace{}, fmt.Errorf("ambiguous cmux workspaces named %q: %s; unknown, never guessed", name, strings.Join(ids, ","))
	}
}

func (b *CmuxBackend) namedWorkspaces(ctx context.Context, name string) ([]cmuxListedWorkspace, error) {
	records, err := b.listWorkspaceRecords(ctx)
	if err != nil {
		return nil, err
	}
	return namedCmuxWorkspaces(records, name), nil
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
	SocketPath      string
	ProtocolVersion int
	ProtocolIsInt   bool
}

type cmuxListedWorkspace struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	WindowID string `json:"window_id"`
	Selected bool   `json:"selected"`
}

type cmuxHealthSurface struct {
	Type     string `json:"type"`
	InWindow *bool  `json:"in_window"`
}

func parseCmuxCapabilities(raw string) (cmuxCapabilities, error) {
	var parsed struct {
		SocketPath string          `json:"socket_path"`
		Version    json.RawMessage `json:"version"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return cmuxCapabilities{}, fmt.Errorf("parse cmux capabilities: %w", err)
	}
	if strings.TrimSpace(parsed.SocketPath) == "" {
		return cmuxCapabilities{}, fmt.Errorf("cmux capabilities missing socket_path")
	}
	caps := cmuxCapabilities{SocketPath: parsed.SocketPath}
	version := bytes.TrimSpace(parsed.Version)
	if len(version) == 0 || string(version) == "null" {
		return cmuxCapabilities{}, fmt.Errorf("cmux capabilities missing version")
	}
	if version[0] == '"' {
		return caps, nil
	}
	if err := json.Unmarshal(version, &caps.ProtocolVersion); err != nil {
		return cmuxCapabilities{}, fmt.Errorf("parse cmux capabilities version: %w", err)
	}
	caps.ProtocolIsInt = true
	return caps, nil
}

func parseCmuxWorkspaceList(raw string) ([]cmuxListedWorkspace, error) {
	var parsed struct {
		WindowID   string                `json:"window_id"`
		Workspaces []cmuxListedWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse cmux workspaces: %w", err)
	}
	if parsed.Workspaces == nil {
		return nil, fmt.Errorf("cmux workspaces list is missing")
	}
	envelopeWindow := strings.TrimSpace(parsed.WindowID)
	if envelopeWindow != "" {
		id, err := normalizeCmuxUUID(envelopeWindow)
		if err != nil {
			return nil, fmt.Errorf("cmux workspaces window_id: %w", err)
		}
		envelopeWindow = id
	}
	for i, workspace := range parsed.Workspaces {
		id, err := normalizeCmuxUUID(workspace.ID)
		if err != nil {
			return nil, fmt.Errorf("cmux workspaces[%d]: %w", i, err)
		}
		parsed.Workspaces[i].ID = id
		windowID := strings.TrimSpace(workspace.WindowID)
		if windowID == "" {
			parsed.Workspaces[i].WindowID = envelopeWindow
			continue
		}
		windowID, err = normalizeCmuxUUID(windowID)
		if err != nil {
			return nil, fmt.Errorf("cmux workspaces[%d] window_id: %w", i, err)
		}
		parsed.Workspaces[i].WindowID = windowID
	}
	return parsed.Workspaces, nil
}

func parseCmuxOKWorkspaceAck(raw string) error {
	if strings.Contains(strings.TrimSuffix(raw, "\n"), "\n") {
		return fmt.Errorf("parse cmux new-workspace: extra line")
	}
	line := strings.TrimSpace(raw)
	if !strings.HasPrefix(line, "OK workspace:") {
		return fmt.Errorf("parse cmux new-workspace: want OK workspace: ack, got %q", line)
	}
	if strings.TrimSpace(strings.TrimPrefix(line, "OK workspace:")) == "" {
		return fmt.Errorf("parse cmux new-workspace: missing workspace ref")
	}
	return nil
}

func parseCmuxSplitSurface(raw string) (string, error) {
	var parsed struct {
		SurfaceID string `json:"surface_id"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", fmt.Errorf("parse cmux new-split: %w", err)
	}
	id, err := normalizeCmuxUUID(parsed.SurfaceID)
	if err != nil {
		return "", fmt.Errorf("cmux new-split surface_id: %w", err)
	}
	return id, nil
}

func parseCmuxPaneIDs(raw string) ([]string, error) {
	var parsed struct {
		Panes []struct {
			ID string `json:"id"`
		} `json:"panes"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse cmux panes: %w", err)
	}
	if parsed.Panes == nil {
		return nil, fmt.Errorf("cmux panes list is missing")
	}
	ids := make([]string, 0, len(parsed.Panes))
	for i, pane := range parsed.Panes {
		id, err := normalizeCmuxUUID(pane.ID)
		if err != nil {
			return nil, fmt.Errorf("cmux panes[%d]: %w", i, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseCmuxTerminalSurfaceIDs(raw string) ([]string, error) {
	var parsed struct {
		Surfaces []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"surfaces"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse cmux pane surfaces: %w", err)
	}
	if parsed.Surfaces == nil {
		return nil, fmt.Errorf("cmux pane surfaces list is missing")
	}
	var ids []string
	for i, surface := range parsed.Surfaces {
		if surface.Type != "terminal" {
			continue
		}
		id, err := normalizeCmuxUUID(surface.ID)
		if err != nil {
			return nil, fmt.Errorf("cmux pane surfaces[%d]: %w", i, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func cmuxSurfacesHealthy(raw string, want int) (bool, bool, error) {
	var parsed struct {
		Surfaces []cmuxHealthSurface `json:"surfaces"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return false, false, fmt.Errorf("parse cmux surface-health: %w", err)
	}
	if parsed.Surfaces == nil {
		return false, false, fmt.Errorf("cmux surface-health list is missing")
	}
	var terminals []cmuxHealthSurface
	for _, surface := range parsed.Surfaces {
		if surface.Type != "" && surface.Type != "terminal" {
			continue
		}
		terminals = append(terminals, surface)
	}
	if len(terminals) != want {
		return false, false, nil
	}
	needSelect := false
	for i, surface := range terminals {
		if surface.InWindow == nil {
			return false, false, fmt.Errorf("cmux surface-health[%d]: in_window is required", i)
		}
		if !*surface.InWindow {
			needSelect = true
		}
	}
	if needSelect {
		return false, true, nil
	}
	return true, false, nil
}

func namedCmuxWorkspaces(records []cmuxListedWorkspace, name string) []cmuxListedWorkspace {
	var matches []cmuxListedWorkspace
	for _, workspace := range records {
		if workspace.Title == name {
			matches = append(matches, workspace)
		}
	}
	return matches
}

func countNamedCmuxWorkspaces(records []cmuxListedWorkspace, name string) int {
	return len(namedCmuxWorkspaces(records, name))
}

func cmuxSelectedWorkspaceID(records []cmuxListedWorkspace) string {
	for _, workspace := range records {
		if workspace.Selected {
			return strings.ToLower(workspace.ID)
		}
	}
	return ""
}

func cmuxWorkspaceIDs(records []cmuxListedWorkspace) []string {
	ids := make([]string, 0, len(records))
	for _, workspace := range records {
		ids = append(ids, strings.ToLower(workspace.ID))
	}
	return ids
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

func parseCmuxCLIVersion(raw string) (string, error) {
	if strings.Contains(strings.TrimSuffix(raw, "\n"), "\n") {
		return "", fmt.Errorf("parse cmux version: extra line")
	}
	line := strings.TrimSpace(raw)
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "cmux" {
		return "", fmt.Errorf("parse cmux version: %q", line)
	}
	version := fields[1]
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("parse cmux version: %q", line)
	}
	for _, part := range parts {
		if part == "" {
			return "", fmt.Errorf("parse cmux version: %q", line)
		}
		if _, err := strconv.Atoi(part); err != nil {
			return "", fmt.Errorf("parse cmux version: %q", line)
		}
	}
	rest := fields[2:]
	switch len(rest) {
	case 0:
		return version, nil
	case 1:
		if !strings.HasPrefix(rest[0], "(") || !strings.HasSuffix(rest[0], ")") {
			return "", fmt.Errorf("parse cmux version: %q", line)
		}
		return version, nil
	case 2:
		if !strings.HasPrefix(rest[0], "(") || !strings.HasSuffix(rest[0], ")") ||
			!strings.HasPrefix(rest[1], "[") || !strings.HasSuffix(rest[1], "]") {
			return "", fmt.Errorf("parse cmux version: %q", line)
		}
		return version, nil
	default:
		return "", fmt.Errorf("parse cmux version: %q", line)
	}
}

func supportedCmuxVersion(raw string) bool {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	patch, errPatch := strconv.Atoi(parts[2])
	if errMajor != nil || errMinor != nil || errPatch != nil || major != 0 {
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

func cmuxOwnedSurfaceIDs(binding BindingRecord) []string {
	ids := make([]string, 0)
	for _, resource := range binding.Resources.Resources {
		if resource.Agent == "" {
			continue
		}
		id, err := parseCmuxSurfaceResource(resource.OpaqueID)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (b *CmuxBackend) ownedCmuxSurfacesStillLive(ctx context.Context, workspaceID string, owned []string) ([]string, error) {
	listed, err := b.listWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	if !cmuxContainsID(listed, workspaceID) {
		return nil, nil
	}
	live, err := b.workspaceSurfaces(ctx, workspaceID)
	if err != nil {
		if cmuxEmptyWorkspace(err) {
			return nil, nil
		}
		return nil, err
	}
	liveSet := make(map[string]bool, len(live))
	for _, id := range live {
		liveSet[id] = true
	}
	remaining := make([]string, 0)
	for _, id := range owned {
		if liveSet[id] {
			remaining = append(remaining, id)
		}
	}
	return remaining, nil
}

func cmuxTargetMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "unknown") || strings.Contains(msg, "absent")
}

func cmuxEmptyWorkspace(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "has no panes") || strings.Contains(msg, "has no terminal surfaces")
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
