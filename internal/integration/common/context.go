package common

// OrchestratorContext builds the context.orchestrator JSON object used by all
// integration messages. The returned map is meant to be nested under a
// "orchestrator" key in the message context.
//
// Spec reference: "All integration metadata lives under context.orchestrator."
type OrchestratorContext struct {
	Version   int                    `json:"version"`
	Name      string                 `json:"name"`
	Transport string                 `json:"transport"`
	Event     string                 `json:"event"`
	Workspace *WorkspaceContext      `json:"workspace,omitempty"`
	Task      map[string]interface{} `json:"task,omitempty"`
}

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
