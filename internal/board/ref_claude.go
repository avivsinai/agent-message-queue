package board

// ClaudeSession builds the identity of a Claude Code session. Both harnesses
// use UUID session ids, so the same shape check applies to their refs.
func ClaudeSession(id string) (Ref, bool) {
	if !codexThreadID.MatchString(id) {
		return Ref{}, false
	}
	return Ref{Kind: "claude-session", ID: id}, true
}
