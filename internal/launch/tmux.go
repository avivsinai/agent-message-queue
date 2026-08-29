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
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	tmuxProfileVersion           = 1
	tmuxProfileVersionRange      = ">=3.2 <4.0"
	tmuxNonceEnvironment         = "AMQ_LAUNCH_NONCE"
	tmuxPaneNonceOption          = "@amq_launch_nonce"
	tmuxCommandTimeout           = 5 * time.Second
	tmuxEpochPrefix              = "tmux:v1:epoch:"
	ownedPaneIdentityUnavailable = "owned_pane_identity_unavailable"
	tmuxServerEpochMismatch      = "tmux_server_epoch_mismatch"
)

var (
	errOwnedPaneIdentityUnavailable = errors.New(ownedPaneIdentityUnavailable)
	errTmuxServerEpochMismatch      = errors.New(tmuxServerEpochMismatch)
)

// TmuxBackend manages one deterministic tmux session per AMQ project/session.
// The socket namespace is stable across tmux server restarts; resource IDs are
// still live tmux IDs and are never reconstructed from display names.
type TmuxBackend struct {
	binary           string
	socketName       string
	run              func(context.Context, ...string) (string, error)
	focus            func(context.Context, string) error
	hostname         func() (string, error)
	getenv           func(string) string
	uid              func() string
	sleep            func(context.Context, time.Duration) error
	socketOnce       sync.Once
	socketPathValue  string
	socketFromActive bool
}

func NewTmuxBackend(binary string) *TmuxBackend {
	if strings.TrimSpace(binary) == "" {
		binary = "tmux"
	}
	b := &TmuxBackend{
		binary: binary, hostname: os.Hostname, getenv: os.Getenv, uid: currentUserID,
		sleep: func(ctx context.Context, delay time.Duration) error {
			if delay <= 0 {
				return nil
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
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
	preview, err := ResolvePlacement(LauncherTMux, req.Placement)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	if req.Placement != nil && !preview.Supported {
		return CreateResult{}, &DefinitePreCreateError{Err: placementError(PlacementUnsupportedReason)}
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
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("tmux %s is unavailable", tmuxProfileVersionRange)}
	}
	if req.JoinBinding != nil {
		if req.Placement != nil {
			return CreateResult{}, &DefinitePreCreateError{Err: placementError(PlacementUnsupportedReason)}
		}
		return b.joinExistingSession(req, detect)
	}
	if req.Placement != nil {
		switch req.Placement.Target {
		case PlacementTargetSession:
			return b.createSessionLayout(req, detect, preview)
		case PlacementTargetNewWindow:
			return b.createNewWindowLayout(req, detect, preview)
		case PlacementTargetCurrentWindow:
			return b.createCurrentWindowLayout(req, detect, preview)
		default:
			return CreateResult{}, &DefinitePreCreateError{Err: placementError(PlacementUnsupportedReason)}
		}
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
	out, err := b.runNewSessionWithTransientRetry(ctx, b.args("new-session", "-d", "-P", "-F", "#{session_id}\t#{window_id}\t#{pane_id}",
		"-s", name, "-n", first.Handle, "-e", tmuxNonceEnvironment+"="+first.LaunchNonce, firstLine)...)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create tmux session: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 3 || fields[0] == "" || fields[1] == "" || !tmuxPaneID(fields[2]) {
		return CreateResult{}, fmt.Errorf("tmux create returned invalid resource identities %q", strings.TrimSpace(out))
	}
	sessionID, windowID, paneID := fields[0], fields[1], fields[2]
	if err := b.markWindow(ctx, windowID, first.Handle); err != nil {
		return CreateResult{}, err
	}
	if err := b.markPane(ctx, paneID, first.Handle, first.LaunchNonce); err != nil {
		return CreateResult{}, err
	}
	for _, agent := range req.Plan.Agents[1:] {
		createdWindow, createErr := b.run(ctx, b.args("new-window", "-d", "-P", "-F", "#{window_id}\t#{pane_id}",
			"-t", "="+name, "-n", agent.Handle, b.agentCommand(req, agent))...)
		if createErr != nil {
			return CreateResult{}, fmt.Errorf("create tmux window for %s: %w", agent.Handle, createErr)
		}
		windowFields := strings.Split(strings.TrimSpace(createdWindow), "\t")
		if len(windowFields) != 2 || windowFields[0] == "" || !tmuxPaneID(windowFields[1]) {
			return CreateResult{}, fmt.Errorf("tmux returned no window identity for %s", agent.Handle)
		}
		if err := b.markWindow(ctx, windowFields[0], agent.Handle); err != nil {
			return CreateResult{}, err
		}
		if err := b.markPane(ctx, windowFields[1], agent.Handle, agent.LaunchNonce); err != nil {
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
	return b.createdBinding(detect, first.LaunchNonce, resources, preview)
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
	if err := req.Binding.Validate(); err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if err := b.validateBindingContext(req.Binding); err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	inspection, inspectErr := b.inspect(ctx, req.Binding)
	if inspectErr == nil && inspection.Status == InspectAbsent {
		return CloseResult{Outcome: OutcomeClosed, Reason: "tmux resources already absent"}, nil
	}
	if err := b.checkBindingEpoch(ctx, req.Binding); err != nil {
		reason := err.Error()
		if errors.Is(err, errTmuxServerEpochMismatch) {
			reason = tmuxServerEpochMismatch
		}
		return CloseResult{Outcome: OutcomeActionRequired, Reason: reason}, nil
	}
	owned, err := b.authorizedOwnedPanes(ctx, req.Binding)
	if err != nil {
		reason := err.Error()
		if errors.Is(err, errOwnedPaneIdentityUnavailable) {
			reason = ownedPaneIdentityUnavailable
		}
		return CloseResult{Outcome: OutcomeActionRequired, Reason: reason}, nil
	}
	if len(tmuxOwnedPaneIDs(req.Binding)) > 0 && len(owned) == 0 {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: ownedPaneIdentityUnavailable}, nil
	}
	if err := b.killAuthorizedPanes(ctx, req.Binding, owned); err != nil {
		if errors.Is(err, errOwnedPaneIdentityUnavailable) {
			return CloseResult{Outcome: OutcomeActionRequired, Reason: ownedPaneIdentityUnavailable}, nil
		}
		return CloseResult{}, err
	}
	leftover, err := b.anyOwnedPaneExists(ctx, owned)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if leftover {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: "owned pane still alive after close"}, nil
	}
	if launcher := req.Binding.Placement.Effective.LauncherPane; launcher != "" {
		exists, launcherErr := b.paneExists(ctx, launcher)
		if launcherErr != nil {
			return CloseResult{Outcome: OutcomeActionRequired, Reason: launcherErr.Error()}, nil
		}
		if !exists {
			return CloseResult{Outcome: OutcomeActionRequired, Reason: "launcher pane missing after close"}, nil
		}
	}
	return CloseResult{Outcome: OutcomeClosed, Reason: "tmux panes closed"}, nil
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
		panes := tmuxOwnedPaneIDs(req.Binding)
		if len(panes) == 0 {
			return FocusResult{}, err
		}
		if _, focusErr := b.run(ctx, b.args("select-pane", "-t", panes[0])...); focusErr != nil {
			return FocusResult{}, focusErr
		}
		return FocusResult{Outcome: OutcomeAttached}, nil
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
	target := journalLayoutTarget(req.Journal)
	switch target {
	case PlacementTargetNewWindow, PlacementTargetCurrentWindow:
		return b.reclaimOwnedLayout(ctx, req, target)
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
	if req.Journal.Binding != nil {
		if err := b.checkBindingEpoch(ctx, *req.Journal.Binding); err != nil {
			return ReclaimResult{Status: ReclaimUnknown, Evidence: tmuxServerEpochMismatch}, nil
		}
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
		Placement:   req.Journal.Placement,
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

func (b *TmuxBackend) joinExistingSession(req CreateRequest, detect DetectResult) (CreateResult, error) {
	sessionID, name, err := parseTmuxSessionResource(*req.JoinBinding)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	if err := b.validateJoinSession(ctx, *req.JoinBinding, sessionID, name); err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	nonce := req.JoinBinding.LaunchNonce
	journaled := make(map[string]JoinDelta, len(req.JoinDeltas))
	for _, delta := range req.JoinDeltas {
		journaled[delta.Handle] = delta
	}
	for _, agent := range req.Plan.Agents {
		resources, resourceErr := b.liveResources(ctx, name)
		if resourceErr != nil {
			return CreateResult{}, resourceErr
		}
		if resource := tmuxResourceForAgent(resources, agent.Handle); resource != nil {
			delta, deltaErr := b.joinDelta(ctx, *req.JoinBinding, sessionID, name, nonce, agent.Handle, *resource)
			if deltaErr != nil {
				return CreateResult{}, &DefinitePreCreateError{Err: deltaErr}
			}
			if prior, ok := journaled[agent.Handle]; ok && prior != delta {
				return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("tmux joined delta for %s changed", agent.Handle)}
			}
			if _, ok := journaled[agent.Handle]; !ok && req.JoinProgress != nil {
				if err := req.JoinProgress(delta); err != nil {
					return CreateResult{}, err
				}
			}
			journaled[agent.Handle] = delta
			continue
		}
		if err := b.validateJoinSession(ctx, *req.JoinBinding, sessionID, name); err != nil {
			return CreateResult{}, &DefinitePreCreateError{Err: err}
		}
		createdWindow, createErr := b.run(ctx, b.args("new-window", "-d", "-P", "-F", "#{window_id}\t#{pane_id}",
			"-t", sessionID, "-n", agent.Handle, b.agentCommand(req, agent))...)
		if createErr != nil {
			return CreateResult{}, fmt.Errorf("join tmux window for %s: %w", agent.Handle, createErr)
		}
		windowFields := strings.Split(strings.TrimSpace(createdWindow), "\t")
		if len(windowFields) != 2 || windowFields[0] == "" || !tmuxPaneID(windowFields[1]) {
			return CreateResult{}, fmt.Errorf("tmux returned no window identity for %s", agent.Handle)
		}
		if err := b.validateJoinSession(ctx, *req.JoinBinding, sessionID, name); err != nil {
			return CreateResult{}, &DefinitePreCreateError{Err: err}
		}
		if err := b.markWindow(ctx, windowFields[0], agent.Handle); err != nil {
			return CreateResult{}, err
		}
		if err := b.validateJoinSession(ctx, *req.JoinBinding, sessionID, name); err != nil {
			return CreateResult{}, &DefinitePreCreateError{Err: err}
		}
		if err := b.markPane(ctx, windowFields[1], agent.Handle, nonce); err != nil {
			return CreateResult{}, err
		}
		delta := JoinDelta{Handle: agent.Handle, SessionID: sessionID, SessionName: name, SessionNonce: nonce,
			Epoch: parseTmuxEpoch(*req.JoinBinding), WindowID: windowFields[0], PaneID: windowFields[1]}
		if req.JoinProgress != nil {
			if err := req.JoinProgress(delta); err != nil {
				return CreateResult{}, err
			}
		}
		journaled[agent.Handle] = delta
	}
	resources, err := b.liveResources(ctx, name)
	if err != nil {
		return CreateResult{}, fmt.Errorf("revalidate joined tmux session: %w", err)
	}
	live := map[string]int{}
	for _, resource := range resources {
		if resource.Agent != "" {
			live[resource.Agent]++
		}
	}
	for _, agent := range req.Plan.Agents {
		if live[agent.Handle] != 1 {
			return CreateResult{}, fmt.Errorf("joined tmux inventory missing %s", agent.Handle)
		}
	}
	if resources[0].OpaqueID != tmuxSessionResource(sessionID, name) {
		return CreateResult{}, fmt.Errorf("joined tmux session identity changed before binding")
	}
	return b.createdBinding(detect, nonce, resources, req.JoinBinding.Placement)
}

func (b *TmuxBackend) validateJoinSession(ctx context.Context, binding BindingRecord, sessionID, name string) error {
	if err := b.checkBindingEpoch(ctx, binding); err != nil {
		return err
	}
	present, err := b.hasSession(ctx, name)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("tmux session %q is absent; keep cannot join", name)
	}
	currentID, err := b.sessionIDForName(ctx, name)
	if err != nil {
		return err
	}
	if currentID != sessionID {
		return fmt.Errorf("tmux session identity changed from %s to %s", sessionID, currentID)
	}
	nonce, err := b.sessionNonce(ctx, name)
	if err != nil {
		return err
	}
	if nonce != binding.LaunchNonce {
		return fmt.Errorf("tmux session nonce changed")
	}
	return nil
}

func (b *TmuxBackend) sessionIDForName(ctx context.Context, name string) (string, error) {
	out, err := b.run(ctx, b.args("list-sessions", "-F", "#{session_id}\t#{session_name}")...)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[1] == name && strings.HasPrefix(fields[0], "$") {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("tmux session %q is absent", name)
}

func (b *TmuxBackend) joinDelta(ctx context.Context, binding BindingRecord, sessionID, name, nonce, handle string, resource ResourceIdentity) (JoinDelta, error) {
	paneID, ok := parseTmuxPaneResource(resource.OpaqueID)
	if !ok {
		return JoinDelta{}, fmt.Errorf("tmux joined resource for %s has no pane identity", handle)
	}
	windowID, err := b.paneWindowID(ctx, paneID)
	if err != nil {
		return JoinDelta{}, err
	}
	markerHandle, markerNonce, err := b.paneMarkers(ctx, paneID)
	if err != nil {
		return JoinDelta{}, err
	}
	if markerHandle != handle || markerNonce != nonce {
		return JoinDelta{}, fmt.Errorf("tmux joined pane %s identity changed", paneID)
	}
	return JoinDelta{Handle: handle, SessionID: sessionID, SessionName: name, SessionNonce: nonce,
		Epoch: parseTmuxEpoch(binding), WindowID: windowID, PaneID: paneID}, nil
}

func tmuxResourceForAgent(resources []ResourceIdentity, handle string) *ResourceIdentity {
	for i := range resources {
		if resources[i].Agent == handle {
			resource := resources[i]
			return &resource
		}
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
	if _, _, err := parseTmuxSessionResource(binding); err != nil {
		return b.inspectOwnedLayout(ctx, binding)
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
	if err := b.checkBindingEpoch(ctx, binding); err != nil {
		return InspectResult{Status: InspectUnknown, Evidence: tmuxServerEpochMismatch, ActionRequired: true}, nil
	}
	resources, err := b.liveResources(ctx, name)
	if err != nil {
		return InspectResult{}, err
	}
	want := withoutTmuxEpoch(binding.Resources.Resources)
	if len(resources) != len(want) {
		return InspectResult{Status: InspectUnknown, Evidence: "tmux resource inventory differs from binding", ActionRequired: true}, nil
	}
	live := make(map[string]string, len(resources))
	for _, resource := range resources {
		live[resource.OpaqueID] = resource.Agent
	}
	for _, expected := range want {
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
	paneOut, paneErr := b.run(ctx, b.args("list-panes", "-s", "-t", "="+name, "-F", "#{session_id}\t#{session_name}\t#{pane_id}\t#{@amq_pane_agent}")...)
	if paneErr == nil {
		paneResources, parseErr := parseTmuxPaneInventory(strings.TrimSpace(paneOut), name)
		if parseErr == nil && tmuxAgentCount(paneResources) > 0 {
			return paneResources, nil
		}
	}
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

func (b *TmuxBackend) markOwnedWindow(ctx context.Context, windowID, nonce string) error {
	if _, err := b.run(ctx, b.args("set-option", "-w", "-t", windowID, tmuxPaneNonceOption, nonce)...); err != nil {
		return fmt.Errorf("mark tmux owned window nonce: %w", err)
	}
	return nil
}

func (b *TmuxBackend) paneWindowID(ctx context.Context, paneID string) (string, error) {
	out, err := b.run(ctx, b.args("display-message", "-p", "-t", paneID, "#{window_id}")...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", fmt.Errorf("pane %s has no window identity", paneID)
	}
	return id, nil
}

func (b *TmuxBackend) windowsWithNonce(ctx context.Context, nonce string) ([]string, error) {
	out, err := b.run(ctx, b.args("list-windows", "-a", "-F", "#{window_id}\t#{@amq_launch_nonce}")...)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid tmux window inventory line %q", line)
		}
		if fields[1] == nonce && fields[0] != "" {
			ids = append(ids, fields[0])
		}
	}
	return ids, nil
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
	return commandLine(agent.Cwd, env, coopExecArgv(amq, req.Root.Base(), agent.Handle, agent.Argv, agent.Execution))
}

func (b *TmuxBackend) args(args ...string) []string {
	if b.socketName == "" || b.socketName == "default" {
		if socketPath := b.socketPath(); b.socketFromActive && socketPath != "" {
			return append([]string{"-S", socketPath}, args...)
		}
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
	b.socketOnce.Do(func() {
		b.socketPathValue, b.socketFromActive = b.resolveSocketPath()
	})
	return b.socketPathValue
}

func (b *TmuxBackend) resolveSocketPath() (string, bool) {
	if active := strings.TrimSpace(b.getenv("TMUX")); b.socketName == "" && active != "" {
		if path, _, ok := strings.Cut(active, ","); ok && filepath.IsAbs(path) {
			return filepath.Clean(path), true
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
	return filepath.Join(base, "tmux-"+b.uid(), name), false
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
func tmuxPaneResource(id string) string          { return "tmux:v1:pane:" + id }

func parseTmuxPaneResource(opaque string) (string, bool) {
	id := strings.TrimPrefix(opaque, "tmux:v1:pane:")
	if id == opaque || !tmuxPaneID(id) {
		return "", false
	}
	return id, true
}

func parseTmuxWindowOwned(binding BindingRecord) (string, bool) {
	for _, resource := range binding.Resources.Resources {
		if strings.HasPrefix(resource.OpaqueID, "tmux:v1:window:") && resource.Agent == "" {
			return strings.TrimPrefix(resource.OpaqueID, "tmux:v1:window:"), true
		}
	}
	for _, resource := range binding.Resources.Resources {
		if strings.HasPrefix(resource.OpaqueID, "tmux:v1:window:") {
			return strings.TrimPrefix(resource.OpaqueID, "tmux:v1:window:"), true
		}
	}
	return "", false
}

func tmuxOwnedPaneIDs(binding BindingRecord) []string {
	ids := make([]string, 0, len(binding.Resources.Resources))
	for _, resource := range binding.Resources.Resources {
		if id, ok := parseTmuxPaneResource(resource.OpaqueID); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func tmuxAgentCount(resources []ResourceIdentity) int {
	count := 0
	for _, resource := range resources {
		if resource.Agent != "" {
			count++
		}
	}
	return count
}

func parseTmuxPaneInventory(out, name string) ([]ResourceIdentity, error) {
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("tmux session has no panes")
	}
	resources := make([]ResourceIdentity, 0, len(lines)+1)
	var sessionID string
	for i, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || fields[0] == "" || fields[1] != name || fields[2] == "" {
			return nil, fmt.Errorf("invalid tmux pane inventory line %q", line)
		}
		if i == 0 {
			sessionID = fields[0]
			resources = append(resources, ResourceIdentity{OpaqueID: tmuxSessionResource(sessionID, name)})
		} else if fields[0] != sessionID {
			return nil, fmt.Errorf("tmux inventory crossed session identities")
		}
		if fields[3] != "" {
			resources = append(resources, ResourceIdentity{OpaqueID: tmuxPaneResource(fields[2]), Agent: fields[3]})
		}
	}
	return resources, nil
}

func (b *TmuxBackend) markPane(ctx context.Context, paneID, handle, nonce string) error {
	if _, err := b.run(ctx, b.args("set-option", "-p", "-t", paneID, "@amq_pane_agent", handle)...); err != nil {
		return fmt.Errorf("mark tmux pane for %s: %w", handle, err)
	}
	if _, err := b.run(ctx, b.args("set-option", "-p", "-t", paneID, tmuxPaneNonceOption, nonce)...); err != nil {
		return fmt.Errorf("mark tmux pane nonce for %s: %w", handle, err)
	}
	return nil
}

func (b *TmuxBackend) paneExists(ctx context.Context, paneID string) (bool, error) {
	out, err := b.run(ctx, b.args("display-message", "-p", "-t", paneID, "#{pane_id}")...)
	if err != nil {
		if tmuxTargetMissing(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) == paneID, nil
}

func (b *TmuxBackend) windowExists(ctx context.Context, windowID string) (bool, error) {
	out, err := b.run(ctx, b.args("display-message", "-p", "-t", windowID, "#{window_id}")...)
	if err != nil {
		if tmuxTargetMissing(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) == windowID, nil
}

func (b *TmuxBackend) paneIDsInWindow(ctx context.Context, windowID string) (map[string]bool, error) {
	out, err := b.run(ctx, b.args("list-panes", "-t", windowID, "-F", "#{pane_id}")...)
	if err != nil {
		return nil, err
	}
	return tmuxPaneIDSet(out), nil
}

func (b *TmuxBackend) authorizedOwnedPanes(ctx context.Context, binding BindingRecord) ([]string, error) {
	launcher := binding.Placement.Effective.LauncherPane
	expected := tmuxPersistedPaneHandles(binding)
	agents := tmuxBindingAgents(binding)
	candidates := make(map[string]string)
	for id, handle := range expected {
		if id != "" && id != launcher {
			candidates[id] = handle
		}
	}
	for _, windowID := range tmuxBoundWindowIDs(binding) {
		live, err := b.paneIDsInWindow(ctx, windowID)
		if err != nil {
			if tmuxTargetMissing(err) {
				continue
			}
			return nil, err
		}
		for id := range live {
			if id == "" || id == launcher {
				continue
			}
			if _, ok := candidates[id]; !ok {
				candidates[id] = ""
			}
		}
	}
	owned := make([]string, 0, len(candidates))
	for id, expectedHandle := range candidates {
		handle, nonce, err := b.paneMarkers(ctx, id)
		if err != nil {
			if tmuxTargetMissing(err) {
				continue
			}
			return nil, err
		}
		if handle == "" || nonce != binding.LaunchNonce {
			continue
		}
		if expectedHandle != "" && handle != expectedHandle {
			continue
		}
		if expectedHandle == "" && !agents[handle] {
			continue
		}
		owned = append(owned, id)
	}
	if len(owned) == 0 && len(expected) == 0 {
		return nil, errOwnedPaneIdentityUnavailable
	}
	return owned, nil
}

func (b *TmuxBackend) paneMarkers(ctx context.Context, paneID string) (string, string, error) {
	out, err := b.run(ctx, b.args("display-message", "-p", "-t", paneID, "#{@amq_pane_agent}\t#{@amq_launch_nonce}")...)
	if err != nil {
		return "", "", err
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	handle, nonce := "", ""
	if len(fields) > 0 {
		handle = fields[0]
	}
	if len(fields) > 1 {
		nonce = fields[1]
	}
	return handle, nonce, nil
}

func (b *TmuxBackend) paneAuthenticated(ctx context.Context, paneID, expectedHandle, nonce string) (bool, error) {
	handle, liveNonce, err := b.paneMarkers(ctx, paneID)
	if err != nil {
		if tmuxTargetMissing(err) {
			return false, nil
		}
		return false, err
	}
	if handle == "" || liveNonce != nonce {
		return false, nil
	}
	if expectedHandle != "" && handle != expectedHandle {
		return false, nil
	}
	return true, nil
}

func (b *TmuxBackend) killAuthorizedPanes(ctx context.Context, binding BindingRecord, panes []string) error {
	expected := tmuxPersistedPaneHandles(binding)
	for _, pane := range panes {
		ok, err := b.paneAuthenticated(ctx, pane, expected[pane], binding.LaunchNonce)
		if err != nil {
			return err
		}
		if !ok {
			return errOwnedPaneIdentityUnavailable
		}
		if _, err := b.run(ctx, b.args("kill-pane", "-t", pane)...); err != nil && !tmuxTargetMissing(err) {
			return fmt.Errorf("close tmux pane %s: %w", pane, err)
		}
	}
	return nil
}

func (b *TmuxBackend) checkBindingEpoch(ctx context.Context, binding BindingRecord) error {
	want := parseTmuxEpoch(binding)
	if want == "" {
		return nil
	}
	got, err := b.serverEpoch(ctx)
	if err != nil {
		if tmuxTargetMissing(err) {
			return errTmuxServerEpochMismatch
		}
		return err
	}
	if got != want {
		return errTmuxServerEpochMismatch
	}
	return nil
}

func (b *TmuxBackend) serverEpoch(ctx context.Context) (string, error) {
	out, err := b.run(ctx, b.args("display-message", "-p", "#{pid}")...)
	if err != nil {
		out, err = b.run(ctx, b.args("list-sessions", "-F", "#{pid}")...)
		if err != nil {
			return "", err
		}
	}
	pid := strings.TrimSpace(out)
	if i := strings.IndexByte(pid, '\n'); i >= 0 {
		pid = strings.TrimSpace(pid[:i])
	}
	if pid == "" || pid == "0" {
		return "", fmt.Errorf("tmux server pid is missing")
	}
	return pid, nil
}

func (b *TmuxBackend) withServerEpoch(ctx context.Context, resources []ResourceIdentity) ([]ResourceIdentity, error) {
	if parseTmuxEpoch(BindingRecord{Resources: ResourceIdentitySet{Resources: resources}}) != "" {
		return resources, nil
	}
	epoch, err := b.serverEpoch(ctx)
	if err != nil {
		return nil, fmt.Errorf("persist tmux server epoch: %w", err)
	}
	return append(append([]ResourceIdentity(nil), resources...), ResourceIdentity{OpaqueID: tmuxEpochPrefix + epoch}), nil
}

func parseTmuxEpoch(binding BindingRecord) string {
	for _, resource := range binding.Resources.Resources {
		if resource.Agent != "" {
			continue
		}
		if id, ok := strings.CutPrefix(resource.OpaqueID, tmuxEpochPrefix); ok && id != "" {
			return id
		}
	}
	return ""
}

func withoutTmuxEpoch(resources []ResourceIdentity) []ResourceIdentity {
	out := make([]ResourceIdentity, 0, len(resources))
	for _, resource := range resources {
		if resource.Agent == "" && strings.HasPrefix(resource.OpaqueID, tmuxEpochPrefix) {
			continue
		}
		out = append(out, resource)
	}
	return out
}

func tmuxPersistedPaneHandles(binding BindingRecord) map[string]string {
	handles := make(map[string]string)
	for _, resource := range binding.Resources.Resources {
		id, ok := parseTmuxPaneResource(resource.OpaqueID)
		if !ok || resource.Agent == "" {
			continue
		}
		handles[id] = resource.Agent
	}
	return handles
}

func tmuxBindingAgents(binding BindingRecord) map[string]bool {
	agents := make(map[string]bool)
	for _, resource := range binding.Resources.Resources {
		if resource.Agent != "" {
			agents[resource.Agent] = true
		}
	}
	return agents
}

func tmuxBoundWindowIDs(binding BindingRecord) []string {
	ids := make([]string, 0)
	seen := make(map[string]bool)
	for _, resource := range binding.Resources.Resources {
		if !strings.HasPrefix(resource.OpaqueID, "tmux:v1:window:") {
			continue
		}
		id := strings.TrimPrefix(resource.OpaqueID, "tmux:v1:window:")
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func tmuxPaneIDSet(out string) map[string]bool {
	ids := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		id := strings.TrimSpace(line)
		if id != "" {
			ids[id] = true
		}
	}
	return ids
}

func tmuxTargetMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "couldn't find") ||
		strings.Contains(msg, "can't find") ||
		strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "server exited unexpectedly")
}

// tmuxServerExitedUnexpectedly reports the transient tmux server startup
// race where `new-session` fails with "server exited unexpectedly" (the server
// process exited before completing the command). This is not a missing-target
// condition (see tmuxTargetMissing); it is a racy server launch that succeeds
// on an immediate retry. Used only at the create `new-session` sites, where a
// failure is provably pre-resource-creation and the retry is idempotent.
func tmuxServerExitedUnexpectedly(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "server exited unexpectedly")
}

// tmuxNewSessionTransientBackoff is the bounded wait before a single retry of
// a `new-session` that hit the server-exited race. It is intentionally short
// and unconfigurable: the retry is a one-off race breaker, not a policy.
const tmuxNewSessionTransientBackoff = 50 * time.Millisecond

// runNewSessionWithTransientRetry executes a `new-session` create command,
// retrying exactly once when tmux reports the transient "server exited
// unexpectedly" startup race. Any other error (including a second occurrence)
// is returned unwrapped-by-retry so the caller surfaces its original
// `create tmux session` refusal. The retry is bounded to one attempt and
// honors ctx cancellation during the backoff.
func (b *TmuxBackend) runNewSessionWithTransientRetry(ctx context.Context, args ...string) (string, error) {
	out, err := b.run(ctx, args...)
	if err == nil || !tmuxServerExitedUnexpectedly(err) {
		return out, err
	}
	// Transient server-exit race: retry once after a bounded backoff.
	select {
	case <-time.After(tmuxNewSessionTransientBackoff):
	case <-ctx.Done():
		return out, err
	}
	return b.run(ctx, args...)
}

func tmuxCreateTimeout(req CreateRequest) time.Duration {
	includeFirst := req.Placement != nil && req.Placement.Target == PlacementTargetCurrentWindow
	return tmuxCommandTimeout + placementStaggerBudget(req.Placement, len(req.Plan.Agents), includeFirst)
}

func (b *TmuxBackend) reclaimOwnedLayout(ctx context.Context, req ReclaimRequest, target string) (ReclaimResult, error) {
	if req.Journal.Binding != nil {
		if err := b.checkBindingEpoch(ctx, *req.Journal.Binding); err != nil {
			return ReclaimResult{Status: ReclaimUnknown, Evidence: tmuxServerEpochMismatch}, nil
		}
	}
	out, err := b.run(ctx, b.args("list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{@amq_pane_agent}\t#{@amq_launch_nonce}")...)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	type markedPane struct {
		window string
		pane   string
		agent  string
	}
	found := make([]markedPane, 0)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return ReclaimResult{Status: ReclaimUnknown, Evidence: "invalid tmux pane inventory line " + line}, nil
		}
		if fields[3] != req.Journal.LaunchNonce || fields[2] == "" {
			continue
		}
		found = append(found, markedPane{window: fields[0], pane: fields[1], agent: fields[2]})
	}
	resources := make([]ResourceIdentity, 0, len(found)+1)
	for _, item := range found {
		resources = append(resources, ResourceIdentity{OpaqueID: tmuxPaneResource(item.pane), Agent: item.agent})
	}
	if target == PlacementTargetNewWindow {
		ownedWindows, winErr := b.windowsWithNonce(ctx, req.Journal.LaunchNonce)
		if winErr != nil {
			return ReclaimResult{Status: ReclaimUnknown, Evidence: winErr.Error(), Resources: resources}, nil
		}
		switch len(ownedWindows) {
		case 0:
			if len(found) == 0 {
				return ReclaimResult{Status: ReclaimAbsent, Evidence: "tmux owned window and panes for this generation are absent"}, nil
			}
			return ReclaimResult{Status: ReclaimIncomplete, Evidence: "owned window is absent while panes remain", Resources: resources}, nil
		case 1:
			windowID := ownedWindows[0]
			inWindow, paneErr := b.paneIDsInWindow(ctx, windowID)
			if paneErr != nil {
				return ReclaimResult{Status: ReclaimUnknown, Evidence: paneErr.Error(), Resources: resources}, nil
			}
			owned := make([]string, 0, len(found))
			for _, item := range found {
				owned = append(owned, item.pane)
			}
			status, evidence := tmuxWindowInventoryStatus(inWindow, owned, nil)
			if status == InspectPresent {
				resources = append([]ResourceIdentity{{OpaqueID: tmuxWindowResource(windowID)}}, resources...)
				return b.adoptOwnedLayout(req, resources, 1, "tmux created window and nonce match the complete journal plan")
			}
			if status == InspectAbsent && len(found) == 0 {
				return ReclaimResult{Status: ReclaimAbsent, Evidence: evidence, Resources: resources}, nil
			}
			return ReclaimResult{Status: ReclaimIncomplete, Evidence: evidence, Resources: resources}, nil
		default:
			return ReclaimResult{Status: ReclaimIncomplete, Evidence: "multiple windows carry this launch nonce", Resources: resources}, nil
		}
	}
	launcher := req.Journal.Placement.Effective.LauncherPane
	if launcher == "" {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: "current_window journal has no launcher pane", Resources: resources}, nil
	}
	exists, launcherErr := b.paneExists(ctx, launcher)
	if launcherErr != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: launcherErr.Error(), Resources: resources}, nil
	}
	if !exists {
		if len(found) == 0 {
			return ReclaimResult{Status: ReclaimAbsent, Evidence: "tmux launcher and owned panes for this generation are absent"}, nil
		}
		return ReclaimResult{Status: ReclaimIncomplete, Evidence: "launcher pane is absent while panes remain", Resources: resources}, nil
	}
	launcherWindow, windowErr := b.paneWindowID(ctx, launcher)
	if windowErr != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: windowErr.Error(), Resources: resources}, nil
	}
	inWindow, paneErr := b.paneIDsInWindow(ctx, launcherWindow)
	if paneErr != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: paneErr.Error(), Resources: resources}, nil
	}
	if len(found) == 0 {
		return ReclaimResult{Status: ReclaimAbsent, Evidence: "tmux owned panes for this generation are absent"}, nil
	}
	owned := make([]string, 0, len(found))
	for _, item := range found {
		owned = append(owned, item.pane)
	}
	status, evidence := tmuxWindowInventoryStatus(inWindow, owned, []string{launcher})
	if status == InspectPresent {
		return b.adoptOwnedLayout(req, resources, 0, "tmux owned panes remain in the launcher window")
	}
	if status == InspectAbsent && len(found) == 0 {
		return ReclaimResult{Status: ReclaimAbsent, Evidence: evidence, Resources: resources}, nil
	}
	if status == InspectAbsent {
		evidence = "owned pane is not in the launcher window"
	}
	return ReclaimResult{Status: ReclaimIncomplete, Evidence: evidence, Resources: resources}, nil
}

func (b *TmuxBackend) adoptOwnedLayout(req ReclaimRequest, resources []ResourceIdentity, containers int, evidence string) (ReclaimResult, error) {
	issues := tmuxAgentInventoryIssues(resources, req.Journal.Plan, containers)
	if len(issues) != 0 {
		return ReclaimResult{
			Status:    ReclaimIncomplete,
			Evidence:  "tmux pane inventory differs from plan: " + strings.Join(issues, ","),
			Resources: resources,
		}, nil
	}
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherTMux, HostIdentity: req.Journal.HostIdentity,
		InstanceIdentity: req.Journal.InstanceIdentity, Profile: req.Journal.Profile,
		LaunchNonce: req.Journal.LaunchNonce,
		Resources:   ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources},
		Placement:   req.Journal.Placement,
	}
	host, hostErr := b.hostname()
	instance := "tmux-socket:" + b.socketPath()
	if hostErr != nil || host != binding.HostIdentity || instance != binding.InstanceIdentity || TmuxProfile().Identity() != binding.Profile {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "tmux runtime identity changed", Resources: resources}, nil
	}
	return ReclaimResult{Status: ReclaimAdoptable, Evidence: evidence, Resources: resources, Binding: binding}, nil
}

func (b *TmuxBackend) stagger(ctx context.Context, req CreateRequest, index int) error {
	if b.sleep == nil || req.Placement == nil || req.Placement.StaggerMS <= 0 {
		return nil
	}
	if req.Placement.Target != PlacementTargetCurrentWindow && index == 0 {
		return nil
	}
	return b.sleep(ctx, time.Duration(req.Placement.StaggerMS)*time.Millisecond)
}

func (b *TmuxBackend) createSessionLayout(req CreateRequest, detect DetectResult, preview PlacementPreview) (CreateResult, error) {
	name, err := tmuxSessionName(req.ProjectRoot, req.Session)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCreateTimeout(req))
	defer cancel()
	present, err := b.hasSession(ctx, name)
	if err != nil {
		return CreateResult{}, err
	}
	if present {
		return CreateResult{}, fmt.Errorf("tmux session %q already exists; journal recovery must classify it", name)
	}
	first := req.Plan.Agents[0]
	out, err := b.runNewSessionWithTransientRetry(ctx, b.args("new-session", "-d", "-P", "-F", "#{session_id}\t#{window_id}\t#{pane_id}",
		"-s", name, "-n", first.Handle, "-e", tmuxNonceEnvironment+"="+first.LaunchNonce, b.agentCommand(req, first))...)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create tmux session: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
		return CreateResult{}, fmt.Errorf("tmux create returned invalid resource identities %q", strings.TrimSpace(out))
	}
	sessionID, windowID, paneID := fields[0], fields[1], fields[2]
	if err := b.markPane(ctx, paneID, first.Handle, first.LaunchNonce); err != nil {
		return CreateResult{}, err
	}
	panes := []ResourceIdentity{{OpaqueID: tmuxPaneResource(paneID), Agent: first.Handle}}
	for i, agent := range req.Plan.Agents[1:] {
		if err := b.stagger(ctx, req, i+1); err != nil {
			return CreateResult{}, err
		}
		splitOut, splitErr := b.run(ctx, b.args("split-window", tmuxSplitFlag(req.Placement.Layout), "-t", paneID, "-P", "-F", "#{pane_id}", b.agentCommand(req, agent))...)
		if splitErr != nil {
			return CreateResult{}, fmt.Errorf("create tmux pane for %s: %w", agent.Handle, splitErr)
		}
		paneID = strings.TrimSpace(splitOut)
		if !tmuxPaneID(paneID) {
			return CreateResult{}, fmt.Errorf("tmux returned no pane identity for %s", agent.Handle)
		}
		if err := b.markPane(ctx, paneID, agent.Handle, agent.LaunchNonce); err != nil {
			return CreateResult{}, err
		}
		panes = append(panes, ResourceIdentity{OpaqueID: tmuxPaneResource(paneID), Agent: agent.Handle})
	}
	if _, err := b.run(ctx, b.args("select-layout", "-t", windowID, tmuxSelectLayout(req.Placement.Layout))...); err != nil {
		return CreateResult{}, fmt.Errorf("select tmux layout: %w", err)
	}
	resources := append([]ResourceIdentity{{OpaqueID: tmuxSessionResource(sessionID, name)}}, panes...)
	if issues := tmuxInventoryIssues(resources, req.Plan); len(issues) != 0 {
		return CreateResult{}, fmt.Errorf("created tmux inventory differs from plan: %s", strings.Join(issues, ","))
	}
	return b.createdBinding(detect, first.LaunchNonce, resources, preview)
}

func (b *TmuxBackend) createNewWindowLayout(req CreateRequest, detect DetectResult, preview PlacementPreview) (CreateResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCreateTimeout(req))
	defer cancel()
	sessionID, err := b.currentSessionID(ctx)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	first := req.Plan.Agents[0]
	out, err := b.run(ctx, b.args("new-window", "-d", "-P", "-F", "#{window_id}\t#{pane_id}",
		"-t", sessionID, "-n", first.Handle, b.agentCommand(req, first))...)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create tmux window: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 2 || fields[0] == "" || !tmuxPaneID(fields[1]) {
		return CreateResult{}, fmt.Errorf("tmux create returned invalid window identities %q", strings.TrimSpace(out))
	}
	windowID, paneID := fields[0], fields[1]
	if err := b.markOwnedWindow(ctx, windowID, first.LaunchNonce); err != nil {
		return CreateResult{}, err
	}
	if err := b.markPane(ctx, paneID, first.Handle, first.LaunchNonce); err != nil {
		return CreateResult{}, err
	}
	panes := []ResourceIdentity{{OpaqueID: tmuxPaneResource(paneID), Agent: first.Handle}}
	for i, agent := range req.Plan.Agents[1:] {
		if err := b.stagger(ctx, req, i+1); err != nil {
			return CreateResult{}, err
		}
		splitOut, splitErr := b.run(ctx, b.args("split-window", tmuxSplitFlag(req.Placement.Layout), "-t", paneID, "-P", "-F", "#{pane_id}", b.agentCommand(req, agent))...)
		if splitErr != nil {
			return CreateResult{}, fmt.Errorf("create tmux pane for %s: %w", agent.Handle, splitErr)
		}
		paneID = strings.TrimSpace(splitOut)
		if err := b.markPane(ctx, paneID, agent.Handle, agent.LaunchNonce); err != nil {
			return CreateResult{}, err
		}
		panes = append(panes, ResourceIdentity{OpaqueID: tmuxPaneResource(paneID), Agent: agent.Handle})
	}
	if _, err := b.run(ctx, b.args("select-layout", "-t", windowID, tmuxSelectLayout(req.Placement.Layout))...); err != nil {
		return CreateResult{}, err
	}
	resources := append([]ResourceIdentity{{OpaqueID: tmuxWindowResource(windowID)}}, panes...)
	return b.createdBinding(detect, first.LaunchNonce, resources, preview)
}

func (b *TmuxBackend) createCurrentWindowLayout(req CreateRequest, detect DetectResult, preview PlacementPreview) (CreateResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCreateTimeout(req))
	defer cancel()
	launcher := req.Placement.LauncherPane
	exists, err := b.paneExists(ctx, launcher)
	if err != nil {
		return CreateResult{}, err
	}
	if !exists {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("launcher pane %s is absent", launcher)}
	}
	paneID := launcher
	panes := make([]ResourceIdentity, 0, len(req.Plan.Agents))
	for i, agent := range req.Plan.Agents {
		if err := b.stagger(ctx, req, i); err != nil {
			return CreateResult{}, err
		}
		splitOut, splitErr := b.run(ctx, b.args("split-window", tmuxSplitFlag(req.Placement.Layout), "-t", paneID, "-P", "-F", "#{pane_id}", b.agentCommand(req, agent))...)
		if splitErr != nil {
			return CreateResult{}, fmt.Errorf("create tmux pane for %s: %w", agent.Handle, splitErr)
		}
		paneID = strings.TrimSpace(splitOut)
		if !tmuxPaneID(paneID) {
			return CreateResult{}, fmt.Errorf("tmux returned no pane identity for %s", agent.Handle)
		}
		if err := b.markPane(ctx, paneID, agent.Handle, agent.LaunchNonce); err != nil {
			return CreateResult{}, err
		}
		panes = append(panes, ResourceIdentity{OpaqueID: tmuxPaneResource(paneID), Agent: agent.Handle})
	}
	if _, err := b.run(ctx, b.args("select-layout", "-t", paneID, tmuxSelectLayout(req.Placement.Layout))...); err != nil {
		return CreateResult{}, err
	}
	still, err := b.paneExists(ctx, launcher)
	if err != nil || !still {
		return CreateResult{}, fmt.Errorf("create moved or destroyed launcher pane %s", launcher)
	}
	return b.createdBinding(detect, req.Plan.Agents[0].LaunchNonce, panes, preview)
}

func (b *TmuxBackend) createdBinding(detect DetectResult, nonce string, resources []ResourceIdentity, preview PlacementPreview) (CreateResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	resources, err := b.withServerEpoch(ctx, resources)
	if err != nil {
		return CreateResult{}, err
	}
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherTMux, HostIdentity: detect.HostIdentity,
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

func (b *TmuxBackend) currentSessionID(ctx context.Context) (string, error) {
	if strings.TrimSpace(b.getenv("TMUX")) != "" {
		out, err := b.run(ctx, b.args("display-message", "-p", "#{session_id}")...)
		if err != nil {
			return "", fmt.Errorf("resolve current tmux session: %w", err)
		}
		id := strings.TrimSpace(out)
		if id == "" {
			return "", fmt.Errorf("current tmux session is empty")
		}
		return id, nil
	}
	out, err := b.run(ctx, b.args("list-sessions", "-F", "#{session_id}")...)
	if err != nil {
		return "", fmt.Errorf("list tmux sessions: %w", err)
	}
	ids := strings.Fields(out)
	if len(ids) != 1 {
		return "", fmt.Errorf("tmux new_window requires exactly one session on the socket, found %d", len(ids))
	}
	return ids[0], nil
}

func (b *TmuxBackend) inspectOwnedLayout(ctx context.Context, binding BindingRecord) (InspectResult, error) {
	if err := b.checkBindingEpoch(ctx, binding); err != nil {
		return InspectResult{Status: InspectUnknown, Evidence: tmuxServerEpochMismatch, ActionRequired: true}, nil
	}
	authorized, err := b.authorizedOwnedPanes(ctx, binding)
	if err != nil && !errors.Is(err, errOwnedPaneIdentityUnavailable) {
		return InspectResult{}, err
	}
	authSet := make(map[string]bool, len(authorized))
	for _, id := range authorized {
		authSet[id] = true
	}
	for _, pane := range tmuxOwnedPaneIDs(binding) {
		if pane == binding.Placement.Effective.LauncherPane {
			continue
		}
		exists, existErr := b.paneExists(ctx, pane)
		if existErr != nil {
			return InspectResult{}, existErr
		}
		if exists && !authSet[pane] {
			return InspectResult{Status: InspectUnknown, Evidence: "persisted pane identity is not authenticated", ActionRequired: true}, nil
		}
	}
	panes := tmuxOwnedPaneIDs(binding)
	if len(panes) == 0 {
		return InspectResult{}, fmt.Errorf("binding has no valid tmux session identity")
	}
	if windowID, ok := parseTmuxWindowOwned(binding); ok {
		exists, err := b.windowExists(ctx, windowID)
		if err != nil {
			return InspectResult{}, err
		}
		if !exists {
			for _, pane := range panes {
				paneExists, paneErr := b.paneExists(ctx, pane)
				if paneErr != nil {
					return InspectResult{}, paneErr
				}
				if paneExists {
					return InspectResult{Status: InspectUnknown, Evidence: "owned window is absent while panes remain", ActionRequired: true}, nil
				}
			}
			return InspectResult{Status: InspectAbsent, Evidence: "tmux window is absent"}, nil
		}
		inWindow, err := b.paneIDsInWindow(ctx, windowID)
		if err != nil {
			return InspectResult{}, err
		}
		status, evidence := tmuxWindowInventoryStatus(inWindow, panes, nil)
		if status == InspectAbsent {
			if exists, existErr := b.anyOwnedPaneExists(ctx, panes); existErr != nil {
				return InspectResult{}, existErr
			} else if exists {
				return InspectResult{Status: InspectUnknown, Evidence: "owned pane is not in the bound window", ActionRequired: true}, nil
			}
		}
		if status != InspectPresent {
			return InspectResult{Status: status, Evidence: evidence, ActionRequired: status == InspectUnknown}, nil
		}
	} else {
		launcher := binding.Placement.Effective.LauncherPane
		if launcher == "" {
			return InspectResult{}, fmt.Errorf("binding has no launcher pane or owned window")
		}
		exists, err := b.paneExists(ctx, launcher)
		if err != nil {
			return InspectResult{}, err
		}
		if !exists {
			for _, pane := range panes {
				paneExists, paneErr := b.paneExists(ctx, pane)
				if paneErr != nil {
					return InspectResult{}, paneErr
				}
				if paneExists {
					return InspectResult{Status: InspectUnknown, Evidence: "launcher pane is absent while panes remain", ActionRequired: true}, nil
				}
			}
			return InspectResult{Status: InspectAbsent, Evidence: "tmux pane is absent"}, nil
		}
		launcherWindow, err := b.paneWindowID(ctx, launcher)
		if err != nil {
			return InspectResult{}, err
		}
		inWindow, err := b.paneIDsInWindow(ctx, launcherWindow)
		if err != nil {
			return InspectResult{}, err
		}
		status, evidence := tmuxWindowInventoryStatus(inWindow, panes, []string{launcher})
		if status == InspectAbsent {
			if exists, existErr := b.anyOwnedPaneExists(ctx, panes); existErr != nil {
				return InspectResult{}, existErr
			} else if exists {
				return InspectResult{Status: InspectUnknown, Evidence: "owned pane is not in the launcher window", ActionRequired: true}, nil
			}
		}
		if status != InspectPresent {
			return InspectResult{Status: status, Evidence: evidence, ActionRequired: status == InspectUnknown}, nil
		}
	}
	if launcher := binding.Placement.Effective.LauncherPane; launcher != "" {
		exists, err := b.paneExists(ctx, launcher)
		if err != nil {
			return InspectResult{}, err
		}
		if !exists {
			return InspectResult{Status: InspectUnknown, Evidence: "launcher pane is absent", ActionRequired: true}, nil
		}
	}
	return InspectResult{Status: InspectPresent, Evidence: "tmux owned panes match"}, nil
}

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

func (b *TmuxBackend) anyOwnedPaneExists(ctx context.Context, panes []string) (bool, error) {
	for _, pane := range panes {
		exists, err := b.paneExists(ctx, pane)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func tmuxInventoryIssues(resources []ResourceIdentity, plan Plan) []string {
	return tmuxAgentInventoryIssues(resources, plan, 1)
}

func tmuxPaneSetDiff(live map[string]bool, expected []string) (missing, extra []string) {
	want := make(map[string]bool, len(expected))
	for _, id := range expected {
		if id == "" {
			continue
		}
		want[id] = true
		if !live[id] {
			missing = append(missing, id)
		}
	}
	for id := range live {
		if !want[id] {
			extra = append(extra, id)
		}
	}
	return missing, extra
}

func tmuxWindowInventoryStatus(live map[string]bool, owned, extras []string) (InspectStatus, string) {
	expected := make([]string, 0, len(owned)+len(extras))
	expected = append(expected, owned...)
	expected = append(expected, extras...)
	_, extra := tmuxPaneSetDiff(live, expected)
	if len(extra) > 0 {
		return InspectUnknown, "unowned pane is in the owned window"
	}
	ownedLive := 0
	for _, id := range owned {
		if live[id] {
			ownedLive++
		}
	}
	if ownedLive == len(owned) {
		return InspectPresent, "tmux owned panes match"
	}
	if ownedLive == 0 {
		return InspectAbsent, "tmux pane is absent"
	}
	return InspectUnknown, "owned window pane inventory is incomplete"
}

func tmuxAgentInventoryIssues(resources []ResourceIdentity, plan Plan, containers int) []string {
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
	if len(resources) != len(plan.Agents)+containers {
		issues = append(issues, fmt.Sprintf("resource_count:%d", len(resources)))
	}
	return issues
}
