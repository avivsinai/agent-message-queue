package thread

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// CollectNamed reads exact message identities without scanning unrelated mail.
// Every present delivery copy must agree; missing or unreadable evidence is an
// acquisition error, never a reason to fall back to a mailbox scan.
func CollectNamed(root, threadID string, agents, ids []string, includeBody bool) ([]Entry, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("message identities are required")
	}
	for _, agent := range agents {
		if err := fsq.ValidateHandle(agent); err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("empty message identity")
		}
		if err := fsq.ValidateMessageFilename(id + ".md"); err != nil {
			return nil, err
		}
	}
	entries := make([]Entry, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var accepted *format.Message
		for _, agent := range agents {
			for _, leaf := range []fsq.MailboxLeaf{fsq.MailboxInboxNew, fsq.MailboxInboxCur, fsq.MailboxOutboxSent} {
				path := filepath.Join(fsq.AgentMailboxPath(root, agent, leaf), id+".md")
				message, err := format.ReadMessageFile(path)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("read named message %s: %w", path, err)
				}
				if message.Header.ID != id || message.Header.Thread != threadID {
					return nil, fmt.Errorf("named message identity or thread changed: %s", path)
				}
				if accepted != nil && !reflect.DeepEqual(*accepted, message) {
					return nil, fmt.Errorf("conflicting delivery copies for message %s", id)
				}
				accepted = &message
			}
		}
		if accepted == nil {
			return nil, fmt.Errorf("named message not found: %s", id)
		}
		entry := entryFromHeader(accepted.Header)
		if includeBody {
			entry.Body = accepted.Body
		}
		entries = append(entries, entry)
	}
	format.SortByTimestamp(entries)
	return entries, nil
}
