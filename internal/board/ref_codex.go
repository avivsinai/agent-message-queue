package board

import "regexp"

var codexThreadID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CodexGoal builds the identity of the one goal owned by a Codex thread.
func CodexGoal(thread string) (Ref, bool) {
	if !codexThreadID.MatchString(thread) {
		return Ref{}, false
	}
	return Ref{Kind: "codex-goal", ID: thread}, true
}
