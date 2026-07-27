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

type baseBacklog struct {
	Root    string
	Session string
	Agent   string
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
	base := baseRootOfForDisplay(root)
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

func findBaseBacklogs(root string, agents []string) []baseBacklog {
	root = absPath(resolveRoot(root))
	base := classifyRoot(root)
	if base == "" {
		return nil
	}
	base = absPath(resolveRoot(base))
	if base == root {
		return nil
	}
	session := sessionName(root)
	if validateSessionName(session) != nil {
		return nil
	}
	resolvedSession, err := resolveSessionRoot(base, session)
	if err != nil || resolvedSession != root {
		return nil
	}

	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		return nil
	}
	baseRoot, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		return nil
	}
	defer func() { _ = baseRoot.Close() }()

	inventory, err := fsq.InspectMailboxLayout(baseRoot)
	if err != nil {
		return nil
	}
	healthy := make(map[string]bool, len(inventory.Mailboxes))
	for _, mailbox := range inventory.Mailboxes {
		if len(mailbox.Issues) == 0 {
			healthy[mailbox.Handle] = true
		}
	}

	seen := make(map[string]bool, len(agents))
	var backlogs []baseBacklog
	for _, agent := range agents {
		normalized, err := normalizeHandle(agent)
		if err != nil || seen[normalized] || !healthy[normalized] {
			continue
		}
		seen[normalized] = true
		entries, err := baseRoot.ReadDir(filepath.Join("agents", normalized, "inbox", "new"))
		if err != nil {
			continue
		}
		pending := countPendingMessageEntries(entries)
		if pending == 0 {
			continue
		}
		backlogs = append(backlogs, baseBacklog{
			Root:    base,
			Session: session,
			Agent:   normalized,
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
	return countPendingMessageEntries(entries), nil
}

func countPendingMessageEntries(entries []os.DirEntry) int {
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
	return count
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
	for _, backlog := range findBaseBacklogs(root, []string{me}) {
		_ = writeStderr("note: %s\n", formatBaseBacklogHint(backlog))
	}
}

func siblingContext(root string) string {
	if session := resolveSessionNameForDisplay(absPath(resolveRoot(root))); session != "" && validateSessionName(session) == nil {
		return session
	}
	return "base root"
}

func formatSiblingBacklogHint(backlog siblingBacklog, me, current string) string {
	return fmt.Sprintf("%d pending for %q in sibling session %q (current: %s); use: "+
		"amq list --session %s --me %s --new",
		backlog.Pending, me, backlog.Session, current, backlog.Session, me)
}

func formatBaseBacklogHint(backlog baseBacklog) string {
	return fmt.Sprintf("%d pending for %q in base root %q (current: %s); use: "+
		"amq list --root %s --me %s --new",
		backlog.Pending, backlog.Agent, backlog.Root, backlog.Session, shellQuoteArg(backlog.Root), backlog.Agent)
}
