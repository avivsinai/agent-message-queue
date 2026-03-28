package common

// BuildOrchestratorLabels builds the standard label set for integration messages.
//
// Label rules from spec:
//   - Always: "orchestrator", "orchestrator:<name>"
//   - When state is known: "task-state:<state>"
//   - Add "handoff" for review-ready / awaiting-review messages
//   - Add "blocking" for failed / interrupted / blocked messages
func BuildOrchestratorLabels(name, state string, flags ...string) []string {
	labels := []string{"orchestrator", "orchestrator:" + name}
	if state != "" {
		labels = append(labels, "task-state:"+state)
	}
	for _, f := range flags {
		if f != "" {
			labels = append(labels, f)
		}
	}
	return labels
}
