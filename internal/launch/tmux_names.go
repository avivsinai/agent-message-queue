package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

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
