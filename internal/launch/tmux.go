package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		sleep: sleepContext,
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
	ctx, cancel := context.WithTimeout(context.Background(), backendCommandTimeout)
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
	if err := validateCreate(req); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), backendCommandTimeout)
	defer cancel()
	present, err := b.hasSession(ctx, name)
	if err != nil {
		return CreateResult{}, err
	}
	if present {
		return CreateResult{}, fmt.Errorf("tmux session %q already exists; journal recovery must classify it", name)
	}

	first := req.Plan.Agents[0]
	firstLine := agentCommand(req, first)
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
			"-t", "="+name, "-n", agent.Handle, agentCommand(req, agent))...)
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
	return inspectBinding(req.Binding, b.inspect)
}

func (b *TmuxBackend) Close(req CloseRequest) (CloseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), backendCommandTimeout)
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
	ctx, cancel := context.WithTimeout(context.Background(), backendCommandTimeout)
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
	ctx, cancel := context.WithTimeout(parent, backendCommandTimeout)
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
