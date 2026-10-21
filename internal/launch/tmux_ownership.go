package launch

import (
	"context"
	"fmt"
	"strings"
)

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
