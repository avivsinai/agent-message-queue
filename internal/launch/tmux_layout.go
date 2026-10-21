package launch

import (
	"context"
	"fmt"
	"strings"
	"time"
)

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
	return backendCommandTimeout + placementStaggerBudget(req.Placement, len(req.Plan.Agents), includeFirst)
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
		"-s", name, "-n", first.Handle, "-e", tmuxNonceEnvironment+"="+first.LaunchNonce, agentCommand(req, first))...)
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
		splitOut, splitErr := b.run(ctx, b.args("split-window", tmuxSplitFlag(req.Placement.Layout), "-t", paneID, "-P", "-F", "#{pane_id}", agentCommand(req, agent))...)
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
		"-t", sessionID, "-n", first.Handle, agentCommand(req, first))...)
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
		splitOut, splitErr := b.run(ctx, b.args("split-window", tmuxSplitFlag(req.Placement.Layout), "-t", paneID, "-P", "-F", "#{pane_id}", agentCommand(req, agent))...)
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
		splitOut, splitErr := b.run(ctx, b.args("split-window", tmuxSplitFlag(req.Placement.Layout), "-t", paneID, "-P", "-F", "#{pane_id}", agentCommand(req, agent))...)
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
	ctx, cancel := context.WithTimeout(context.Background(), backendCommandTimeout)
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
