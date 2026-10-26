package board

import "regexp"

var codexThreadID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CodexGoal identifies a goal UUID. Its containing thread is separate context.
func CodexGoal(goal string) (Ref, bool) {
	if !codexThreadID.MatchString(goal) {
		return Ref{}, false
	}
	return Ref{Kind: "codex-goal", ID: goal}, true
}

// ValidHarnessID validates a native UUID without assigning task identity.
func ValidHarnessID(id string) bool { return codexThreadID.MatchString(id) }
