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
func ReadHistory(ctx context.Context, env board.Env, base string) (board.Result, []board.Message, [][]byte) {
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
	for _, record := range index.Threads {
		for _, root := range record.Roots {
			for _, id := range root.MessageIDs {
				copyFacts[id] = append(copyFacts[id], board.MessageCopy{Root: root.Root, Session: root.Session})
			}
		}
	}
	used := len(raw)
acquire:
	for n, record := range index.Threads {
		if n >= 1000 || used >= 64<<20 {
			result.Coverage.Truncated += len(index.Threads) - n
			gap("history acquisition bound reached")
			break
		}
		for _, root := range record.Roots {
			expected := map[string]bool{}
			for _, id := range root.MessageIDs {
				expected[id] = true
			}
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
	var actionGaps []string
	result.ActionStates, actionGaps = readActionStates(ctx, env, base, messages, result.Status == board.StatusOK)
	for _, detail := range actionGaps {
		gap(detail)
	}
	return result, messages, blobs
}

// readActionStates consumes positive recipient-action facts from AMQ's reducer.
// An omitted opener, continuation, unresolved route or failed acquisition stays
// unknown. No action is closed because it disappeared from an open list.
func readActionStates(ctx context.Context, env board.Env, base string, messages []board.Message, historyComplete bool) (map[string]string, []string) {
	states := map[string]string{}
	for _, message := range messages {
		states[message.Key] = "unknown"
	}
	raw, err := env.Run.RunLimited(ctx, env.Dir, []string{"amq", "thread", "open", "--all-sessions", "--include-base", "--max-age", "0", "--json"}, 64<<20)
	if err != nil {
		return states, []string{"action lifecycle acquisition failed: " + errLine(err)}
	}
	var doc struct {
		Schema   string   `json:"schema"`
		BaseRoot string   `json:"base_root"`
		Sessions []string `json:"sessions"`
		Coverage struct {
			Outcomes *[]struct {
				OpenerID string `json:"opener_id"`
				Key      string `json:"key"`
				State    string `json:"state"`
			} `json:"action_outcomes"`
			MaxAge  int64             `json:"max_age_seconds"`
			Since   string            `json:"assessed_since"`
			Skipped []json.RawMessage `json:"skipped"`
			Pruned  []struct {
				Count int `json:"count"`
			} `json:"pruned_files"`
			Unresolved []struct {
				ID string `json:"id"`
			} `json:"unresolved_lineage"`
			Ambiguous []struct {
				ID string `json:"id"`
			} `json:"ambiguous"`
			Outbound *[]struct {
				ID string `json:"id"`
			} `json:"outbound_routed_openers"`
			Truncated struct {
				Omitted int `json:"omitted"`
			} `json:"truncated_by_limit"`
		} `json:"coverage"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Schema != ThreadOpenSchema || filepath.Clean(doc.BaseRoot) != base || doc.Coverage.Outcomes == nil || doc.Coverage.Outbound == nil {
		return states, []string{"action lifecycle schema or root not accepted"}
	}
	complete := historyComplete && doc.Coverage.MaxAge == 0 && doc.Coverage.Since == "" && doc.Coverage.Truncated.Omitted == 0 && len(doc.Coverage.Skipped) == 0
	for _, row := range doc.Coverage.Pruned {
		if row.Count > 0 {
			complete = false
		}
	}
	var gaps []string
	if !complete {
		gaps = append(gaps, "action lifecycle acquisition has incomplete history coverage")
	}
	covered := map[string]bool{}
	for _, session := range doc.Sessions {
		covered[session] = true
	}
	uncertain := map[string]bool{}
	for _, row := range doc.Coverage.Unresolved {
		uncertain[row.ID] = true
	}
	for _, row := range doc.Coverage.Ambiguous {
		uncertain[row.ID] = true
	}
	for _, row := range *doc.Coverage.Outbound {
		uncertain[row.ID] = true
	}
	outcomes := map[string]string{}
	keys := map[string]bool{}
	for _, row := range *doc.Coverage.Outcomes {
		if row.OpenerID == "" || row.Key == "" || keys[row.Key] || row.State != "open" && row.State != "closed" {
			return states, append(gaps, "action lifecycle outcome not accepted")
		}
		keys[row.Key] = true
		if row.State == "open" || outcomes[row.OpenerID] == "" {
			outcomes[row.OpenerID] = row.State
		}
	}
	for _, message := range messages {
		status := outcomes[message.ID]
		if status == "open" {
			states[message.Key] = "open"
			continue
		}
		if status != "closed" || !complete || uncertain[message.ID] || len(message.Copies) == 0 {
			continue
		}
		allCovered := true
		for _, copy := range message.Copies {
			if !covered[copy.Session] {
				allCovered = false
			}
		}
		if allCovered {
			states[message.Key] = "resolved"
		}
	}
	return states, gaps
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
