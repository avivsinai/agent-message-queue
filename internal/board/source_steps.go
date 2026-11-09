package board

import "strings"

// SQLite3Missing is the reason a source that reads SQLite gives when the
// sqlite3 command is not installed.
const SQLite3Missing = "sqlite3 not found"

// Claude task list gaps that the owner can act on.
const (
	unboundClaudeListPrefix   = "unbound native task list "
	BoundClaudeListMissingGap = "bound native list missing or unsupported: "
)

// UnboundClaudeListGap is the coverage gap of a Claude task list that has
// tasks and no owner binding. It names the list and its home so the board
// can print the bind command. A list name holds no space.
func UnboundClaudeListGap(home, list string) string {
	return unboundClaudeListPrefix + list + " in " + home
}

// ownerSteps are the owner steps the board detects in a source's reasons,
// and whether it knows that no step exists. A read that did not run, or a
// bound list that went missing, has a cause the board cannot judge.
func ownerSteps(s SourceView) (steps []string, noStep bool) {
	if s.Detail == SQLite3Missing {
		return []string{"Install sqlite3"}, false
	}
	known := s.Status == StatusOK
	for _, part := range strings.Split(s.Coverage.Detail, "; ") {
		if rest, ok := strings.CutPrefix(part, unboundClaudeListPrefix); ok {
			if list, home, ok := strings.Cut(rest, " in "); ok {
				steps = append(steps, "Run "+Quote([]string{"amq-board", "bind-claude-list", "--home", home, "--list", list}))
				continue
			}
		}
		if strings.HasPrefix(part, BoundClaudeListMissingGap) {
			known = false
		}
	}
	return steps, known && len(steps) == 0
}
