package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// ReadHistory uses AMQ's metadata enumeration and existing full-body export.
// A missing export is a coverage gap, including commands that warned but
// returned success. Nothing here consumes mail or infers task identity.
// Message ids that pruned reports are pruned history and are not exported;
// a nil pruned exports all history. A message that held returns is already
// published with its immutable content: it is carried with the currently
// enumerated copies and not exported again, so each pass spends its bounds on
// messages the snapshot lacks and every thread is reached in finite passes.
func ReadHistory(ctx context.Context, env board.Env, base string, pruned func(id string) bool, held func(id string) (board.Message, bool)) (board.Result, []board.Message, [][]byte) {
	ctx, cancel := context.WithTimeout(ctx, board.SourceTimeout)
	defer cancel()
	result := board.Result{Status: board.StatusOK, ObservedAt: env.Now(), Feeds: []board.Column{board.ColumnRequests}}
	gap := func(detail string) {
		result.Status = board.StatusDegraded
		result.Coverage.Unresolved++
		if result.Coverage.Detail != "" {
			result.Coverage.Detail += "; "
		}
		result.Coverage.Detail += detail
	}
	raw, err := env.Run.RunLimited(ctx, env.Dir, []string{"amq", "thread", "list", "--all-sessions", "--json"}, 64<<20)
	if err != nil {
		gap("history enumeration failed: " + errLine(err))
		return result, nil, nil
	}
	var index struct {
		Schema   string `json:"schema"`
		BaseRoot string `json:"base_root"`
		Threads  []struct {
			ID         string   `json:"thread_id"`
			MessageIDs []string `json:"message_ids"`
			Roots      []struct {
				Root       string   `json:"root"`
				Session    string   `json:"session"`
				Agents     []string `json:"agents"`
				MessageIDs []string `json:"message_ids"`
			} `json:"roots"`
		} `json:"threads"`
		Coverage struct {
			Complete bool              `json:"complete"`
			Skipped  []json.RawMessage `json:"skipped"`
		} `json:"coverage"`
	}
	if err = json.Unmarshal(raw, &index); err != nil || index.Schema != "amq/thread-list/v1" || filepath.Clean(index.BaseRoot) != base {
		gap("history enumeration schema or root not accepted")
		return result, nil, nil
	}
	if !index.Coverage.Complete {
		gap(fmt.Sprintf("history enumeration skipped %d records", len(index.Coverage.Skipped)))
	}
	contentByID := map[string]string{}
	var blobs [][]byte
	messagesByID := map[string]board.Message{}
	copyFacts := map[string][]board.MessageCopy{}
	if pruned != nil {
		threads := index.Threads[:0]
		for _, record := range index.Threads {
			roots := record.Roots[:0]
			for _, root := range record.Roots {
				ids := root.MessageIDs[:0]
				for _, id := range root.MessageIDs {
					if !pruned(id) {
						ids = append(ids, id)
					}
				}
				if root.MessageIDs = ids; len(ids) > 0 {
					roots = append(roots, root)
				}
			}
			if record.Roots = roots; len(roots) > 0 {
				threads = append(threads, record)
			}
		}
		index.Threads = threads
	}
	for _, record := range index.Threads {
		for _, root := range record.Roots {
			for _, id := range root.MessageIDs {
				copyFacts[id] = append(copyFacts[id], board.MessageCopy{Root: root.Root, Session: root.Session, Agents: append([]string(nil), root.Agents...)})
			}
		}
	}
	heldIDs := map[string]bool{}
	if held != nil {
		for id, copies := range copyFacts {
			if message, ok := held(id); ok {
				message.Copies = copies
				messagesByID[id] = message
				heldIDs[id] = true
			}
		}
	}
	needsExport := func(record int) bool {
		for _, root := range index.Threads[record].Roots {
			for _, id := range root.MessageIDs {
				if !heldIDs[id] {
					return true
				}
			}
		}
		return false
	}
	used := len(raw)
	exporting := 0
acquire:
	for n, record := range index.Threads {
		if !needsExport(n) {
			continue
		}
		if exporting >= 1000 || used >= 64<<20 {
			for m := n; m < len(index.Threads); m++ {
				if needsExport(m) {
					result.Coverage.Truncated++
				}
			}
			gap("history acquisition bound reached")
			break
		}
		exporting++
		for _, root := range record.Roots {
			expected := map[string]bool{}
			var unheld []string
			for _, id := range root.MessageIDs {
				if !heldIDs[id] {
					expected[id] = true
					unheld = append(unheld, id)
				}
			}
			if len(root.MessageIDs) > 0 && len(unheld) == 0 {
				continue
			}
			root.MessageIDs = unheld
			seen := map[string]bool{}
			if len(expected) == 0 {
				gap("history root has no enumerated message identities")
				continue
			}
			rel, err := filepath.Rel(base, root.Root)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				gap("history root outside canonical base")
				continue
			}
			if len(root.Agents) == 0 {
				gap("history root has no agents")
				continue
			}
			batches, err := historyBatches(root.MessageIDs)
			if err != nil {
				gap(err.Error())
				continue
			}
			for _, batch := range batches {
				if ctx.Err() != nil {
					gap("history acquisition deadline reached")
					break acquire
				}
				body, err := env.Run.RunLimited(ctx, env.Dir, []string{"amq", "thread", "--id", record.ID, "--agents", strings.Join(root.Agents, ","), "--message-ids", strings.Join(batch, ","), "--include-body", "--json", "--root", root.Root}, 8<<20)
				if err != nil {
					gap("history export failed: " + errLine(err))
					continue
				}
				var entries []thread.Entry
				if err = json.Unmarshal(body, &entries); err != nil {
					gap("history export unreadable")
					continue
				}
				if used+len(body) > 64<<20 {
					gap("history body bound reached")
					break acquire
				}
				used += len(body)
				for _, entry := range entries {
					if entry.ID == "" || entry.Thread != record.ID || !expected[entry.ID] {
						gap("history message identity not accepted")
						continue
					}
					content, _ := json.Marshal(entry)
					fingerprint := sha256.Sum256(content)
					contentHash := hex.EncodeToString(fingerprint[:])
					if previous, ok := contentByID[entry.ID]; ok && previous != contentHash {
						gap("conflicting content for message ID: " + entry.ID)
						continue
					}
					contentByID[entry.ID] = contentHash
					blobs = append(blobs, content)
					seen[entry.ID] = true
					message, exists := messagesByID[entry.ID]
					if !exists {
						message = board.MessageFromEntry(base, root.Root, root.Session, entry, contentHash)
						message.Copies = nil
					}
					message.Copies = copyFacts[entry.ID]
					messagesByID[entry.ID] = message
				}
			}
			for _, id := range root.MessageIDs {
				if !seen[id] {
					gap("enumerated message body missing in " + root.Root + ": " + id)
				}
			}
		}

	}
	var messages []board.Message
	for _, message := range messagesByID {
		messages = append(messages, message)
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].Key < messages[j].Key })
	return result, messages, blobs
}

// Bound both argument count and UTF-8 bytes below platform command-line limits.
func historyBatches(ids []string) ([][]string, error) {
	var batches [][]string
	var batch []string
	size := 0
	for _, id := range ids {
		n := len(id) + 1
		if n > 8<<10 {
			return nil, fmt.Errorf("history message identity exceeds argument bound")
		}
		if len(batch) >= 128 || size+n > 8<<10 {
			batches = append(batches, batch)
			batch, size = nil, 0
		}
		batch = append(batch, id)
		size += n
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches, nil
}
