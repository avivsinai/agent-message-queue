package launch

import (
	"context"
	"fmt"
	"strings"
)

func (b *TmuxBackend) joinExistingSession(req CreateRequest, detect DetectResult) (CreateResult, error) {
	sessionID, name, err := parseTmuxSessionResource(*req.JoinBinding)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), backendCommandTimeout)
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
			"-t", sessionID, "-n", agent.Handle, agentCommand(req, agent))...)
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
