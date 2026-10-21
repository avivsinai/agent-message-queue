package launch

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

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
	return checkBindingContext(binding, LauncherTMux, TmuxProfile(), b.hostname,
		func() string { return "tmux-socket:" + b.socketPath() }, "")
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
