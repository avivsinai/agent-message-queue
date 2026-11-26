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

// ownerStep is one owner step: a command the owner can copy, or words when
// the step is not a command.
type ownerStep struct {
	Text    string
	Command *Action
}

// ownerSteps are the owner steps the board detects in a source's reasons,
// and whether it knows that no step exists. A read that did not run, or a
// bound list that went missing, has a cause the board cannot judge.
func ownerSteps(s SourceView) (steps []ownerStep, noStep bool) {
	if s.Detail == SQLite3Missing {
		return []ownerStep{{Text: "Install sqlite3"}}, false
	}
	known := s.Status == StatusOK
	for _, part := range strings.Split(s.Coverage.Detail, "; ") {
		if rest, ok := strings.CutPrefix(part, unboundClaudeListPrefix); ok {
			list, home, _ := strings.Cut(rest, " in ")
			// The gap is joined text: a home or list that holds a separator
			// cannot be read back exactly, so it gets no command.
			if action, ok := BindClaudeList(home, list); ok && unambiguousGapField(home) && unambiguousGapField(list) {
				steps = append(steps, ownerStep{Command: &action})
			} else {
				// An unbound list needs the owner even when no safe command can name it.
				known = false
			}
			continue
		}
		if strings.HasPrefix(part, BoundClaudeListMissingGap) {
			known = false
		}
	}
	return steps, known && len(steps) == 0
}

// unambiguousGapField reports that a value holds neither the "; " that joins
// gaps nor the " in " that joins a list to its home.
func unambiguousGapField(v string) bool {
	return !strings.Contains(v, ";") && !strings.Contains(v, " in ")
}
