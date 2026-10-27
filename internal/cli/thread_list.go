package cli

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const threadListSchema = "amq/thread-list/v1"

type threadListResult struct {
	Schema      string             `json:"schema"`
	GeneratedAt string             `json:"generated_at"`
	BaseRoot    string             `json:"base_root"`
	Sessions    []string           `json:"sessions"`
	Threads     []threadListThread `json:"threads"`
	Coverage    threadListCoverage `json:"coverage"`
}

type threadListThread struct {
	ID         string           `json:"thread_id"`
	MessageIDs []string         `json:"message_ids"`
	Count      int              `json:"count"`
	Roots      []threadListRoot `json:"roots"`
}

type threadListRoot struct {
	Session    string   `json:"session"`
	Root       string   `json:"root"`
	Agents     []string `json:"agents"`
	MessageIDs []string `json:"message_ids"`
}

type threadListCoverage struct {
	Messages int                 `json:"messages"`
	Skipped  []threadOpenSkipped `json:"skipped"`
	Complete bool                `json:"complete"`
}

func runThreadList(args []string) error {
	fs := flag.NewFlagSet("thread list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	allSessions := fs.Bool("all-sessions", false, "Scan every canonical session under the base root")
	usage := usageWithFlags(fs, "amq thread list [--all-sessions] [options]",
		"List retained thread IDs and their message IDs.",
		"Read-only: scans inbox/new, inbox/cur, and outbox/sent message headers. It includes closed and withdrawn messages. Use 'amq thread --id <thread_id> --include-body --json' to read message bodies.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	root := absPath(resolveRoot(common.Root))
	if !dirExists(root) {
		return NotFoundError("root not found at %s", root)
	}
	if mismatch, err := sessionPinMismatch(root); err != nil {
		_ = writeStderr("warning: %v\n", err)
	} else if mismatch != nil && !isPinnedBaseRoot(root) {
		_ = writeStderr("warning: %s\n", mismatch.Error())
	}

	baseRoot := absPath(baseRootOf(root))
	session := ""
	if root != baseRoot {
		session = sessionName(root)
	}
	scanRoots := []thread.ScanRoot{{Session: session, Root: root}}
	if *allSessions {
		var err error
		baseRoot, err = sessionBaseRoot(root)
		if err != nil {
			return err
		}
		sessions, err := canonicalSessionRoots(baseRoot)
		if err != nil {
			return err
		}
		scanRoots = append([]thread.ScanRoot{{Root: baseRoot}}, sessions...)
	}

	now := time.Now().UTC()
	result := threadListResult{
		Schema:      threadListSchema,
		GeneratedAt: now.Format(time.RFC3339),
		BaseRoot:    baseRoot,
		Sessions:    make([]string, 0, len(scanRoots)),
		Threads:     []threadListThread{},
		Coverage:    threadListCoverage{Skipped: []threadOpenSkipped{}, Complete: true},
	}
	rootBySession := make(map[string]string, len(scanRoots))
	for _, scanRoot := range scanRoots {
		if !dirExists(scanRoot.Root) {
			return fmt.Errorf("thread list root disappeared before scan: %s", scanRoot.Root)
		}
		result.Sessions = append(result.Sessions, scanRoot.Session)
		rootBySession[scanRoot.Session] = scanRoot.Root
	}

	scan, err := thread.CollectAll(scanRoots, time.Time{}, func(path string, parseErr error) error {
		result.Coverage.Skipped = append(result.Coverage.Skipped, threadOpenSkipped{Path: path, Error: parseErr.Error()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan thread mailboxes: %w", err)
	}
	result.Coverage.Messages = len(scan.Entries)
	result.Coverage.Complete = len(result.Coverage.Skipped) == 0

	byThread := make(map[string]*threadListThread)
	rootAgents := make(map[string]map[string]map[string]bool)
	rootMessageIDs := make(map[string]map[string]map[string]bool)
	for _, entry := range scan.Entries {
		item := byThread[entry.Thread]
		if item == nil {
			item = &threadListThread{ID: entry.Thread, MessageIDs: []string{}}
			byThread[entry.Thread] = item
			rootAgents[entry.Thread] = map[string]map[string]bool{}
			rootMessageIDs[entry.Thread] = map[string]map[string]bool{}
		}
		item.MessageIDs = append(item.MessageIDs, entry.ID)
		for _, occurrence := range entry.Occurrences {
			root := rootBySession[occurrence.Session]
			if root == "" {
				return fmt.Errorf("thread scan returned unknown session %q", occurrence.Session)
			}
			if rootAgents[entry.Thread][occurrence.Session] == nil {
				rootAgents[entry.Thread][occurrence.Session] = map[string]bool{}
				rootMessageIDs[entry.Thread][occurrence.Session] = map[string]bool{}
			}
			rootAgents[entry.Thread][occurrence.Session][occurrence.Agent] = true
			rootMessageIDs[entry.Thread][occurrence.Session][entry.ID] = true
		}
	}
	threadIDs := make([]string, 0, len(byThread))
	for id := range byThread {
		threadIDs = append(threadIDs, id)
	}
	sort.Strings(threadIDs)
	for _, id := range threadIDs {
		item := byThread[id]
		item.Count = len(item.MessageIDs)
		for session, agentSet := range rootAgents[id] {
			agents := make([]string, 0, len(agentSet))
			for agent := range agentSet {
				agents = append(agents, agent)
			}
			sort.Strings(agents)
			messageIDs := make([]string, 0, len(rootMessageIDs[id][session]))
			for messageID := range rootMessageIDs[id][session] {
				messageIDs = append(messageIDs, messageID)
			}
			sort.Strings(messageIDs)
			item.Roots = append(item.Roots, threadListRoot{Session: session, Root: rootBySession[session], Agents: agents, MessageIDs: messageIDs})
		}
		sort.Slice(item.Roots, func(i, j int) bool {
			if item.Roots[i].Session != item.Roots[j].Session {
				return item.Roots[i].Session < item.Roots[j].Session
			}
			return item.Roots[i].Root < item.Roots[j].Root
		})
		result.Threads = append(result.Threads, *item)
	}

	if common.JSON {
		return writeJSON(os.Stdout, result)
	}
	if !result.Coverage.Complete {
		if err := writeStderr("warning: thread list incomplete; skipped %d unreadable message headers\n", len(result.Coverage.Skipped)); err != nil {
			return err
		}
	}
	for _, item := range result.Threads {
		if err := writeStdout("%s  %d messages\n", item.ID, item.Count); err != nil {
			return err
		}
	}
	return nil
}
