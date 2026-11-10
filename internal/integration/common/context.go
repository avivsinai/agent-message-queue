package common

// WorkspaceContext describes the workspace portion of orchestrator context.
type WorkspaceContext struct {
	Path string `json:"path"`
	Key  string `json:"key"`
}

// BuildOrchestratorContext creates a standard orchestrator context object.
// name is the orchestrator name (e.g. "symphony", "kanban").
// transport is "hook" or "bridge".
// event is the lifecycle event name.
// workspace contains path/key info (may be nil).
// task contains task metadata (may be nil).
func BuildOrchestratorContext(name, transport, event string, workspace *WorkspaceContext, task map[string]interface{}) map[string]interface{} {
	orch := map[string]interface{}{
		"version":   1,
		"name":      name,
		"transport": transport,
		"event":     event,
	}
	if workspace != nil {
		orch["workspace"] = map[string]interface{}{
			"path": workspace.Path,
			"key":  workspace.Key,
		}
	}
	if task != nil {
		orch["task"] = task
	}
	return map[string]interface{}{
		"orchestrator": orch,
	}
}
