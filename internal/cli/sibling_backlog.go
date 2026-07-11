package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type siblingBacklog struct {
	Session string
	Pending int
}

// findSiblingBacklogs performs a shallow, best-effort scan under the current
// base root. It counts message-shaped files only; parsing headers here would
// turn an empty-inbox diagnostic into an expensive second validation path.
func findSiblingBacklogs(root, me string) []siblingBacklog {
	normalized, err := normalizeHandle(me)
	if err != nil {
		return nil
	}
	me = normalized
	root = absPath(resolveRoot(root))
	base := baseRootOf(root)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}

	var backlogs []siblingBacklog
	for _, entry := range entries {
		if !entry.IsDir() || validateSessionName(entry.Name()) != nil {
			continue
		}
		candidate := absPath(filepath.Join(base, entry.Name()))
		if candidate == root {
			continue
		}
		pending, err := countPendingMessageFiles(fsq.AgentInboxNew(candidate, me))
		if err != nil || pending == 0 {
			continue
		}
		backlogs = append(backlogs, siblingBacklog{
			Session: entry.Name(),
			Pending: pending,
		})
	}
	return backlogs
}

func countPendingMessageFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".md") {
			count++
		}
	}
	return count, nil
}

func emitSiblingBacklogHintsIfInboxEmpty(root, me string) {
	pending, err := countPendingMessageFiles(fsq.AgentInboxNew(root, me))
	if err != nil || pending != 0 {
		return
	}
	current := siblingContext(root)
	for _, backlog := range findSiblingBacklogs(root, me) {
		_ = writeStderr("note: %s\n", formatSiblingBacklogHint(backlog, me, current))
	}
}

func siblingContext(root string) string {
	if session := resolveSessionName(absPath(resolveRoot(root))); session != "" && validateSessionName(session) == nil {
		return session
	}
	return "base root"
}

func formatSiblingBacklogHint(backlog siblingBacklog, me, current string) string {
	return fmt.Sprintf("%d pending for %q in sibling session %q (current: %s); use: "+
		"amq list --session %s --me %s --new",
		backlog.Pending, me, backlog.Session, current, backlog.Session, me)
}
