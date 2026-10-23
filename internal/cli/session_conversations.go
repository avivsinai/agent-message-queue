package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/execidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// sessionConversation is one harness session that exact AMQ evidence ties to
// an agent of a session. Source names the evidence: launch (a managed launch
// conversation record), codex-thread (the Codex thread coop exec created),
// exec-pid (the Claude Code process coop exec became, joined with Claude's
// sessions/<pid>.json), or remote (an amq-remote binding whose harness
// process is that exec record). State is ready, revoked (a later coop exec
// replaced it), or uncertain. Nothing is inferred from a cwd.
type sessionConversation struct {
	Handle   string `json:"handle"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	State    string `json:"state"`
	Source   string `json:"source"`
}

const (
	conversationReady     = "ready"
	conversationRevoked   = "revoked"
	conversationUncertain = "uncertain"
)

// revokedConversationRetention is how long a revoked record stays listed
// after it was revoked, so usage spent before revocation still attributes.
const revokedConversationRetention = 7 * 24 * time.Hour

// claudeProcStartLayout is the format of procStart in Claude Code's
// sessions/<pid>.json: the ps lstart form, in UTC (Claude Code 2.1.309). The
// format is Claude-private: a value that does not parse joins nothing.
const claudeProcStartLayout = "Mon Jan _2 15:04:05 2006"

var claudeSessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// conversationSources holds the per-user stores read once per listing.
type conversationSources struct {
	now       time.Time
	codex     []codexidentity.Entry
	execs     []execidentity.Entry
	bindings  []binding.Binding
	claudeDir string
}

// loadConversationSources reads the per-user stores. It never writes; a
// store that is missing or unreadable contributes nothing.
func loadConversationSources(now time.Time) conversationSources {
	sources := conversationSources{now: now}
	sources.codex, _ = codexidentity.List()
	sources.execs, _ = execidentity.List()
	sources.bindings, _ = binding.List()
	sources.claudeDir = claudeConfigDir()
	return sources
}

func claudeConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// forSession lists the conversations of the session at sessionRoot.
func (sources conversationSources) forSession(sessionRoot string, agents []string) []sessionConversation {
	out := launchConversations(sessionRoot, agents)
	if rootID, err := resolveTreeIdentityToken(sessionRoot); err == nil {
		out = append(out, sources.codexConversations(rootID)...)
		out = append(out, sources.execConversations(rootID)...)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Handle != b.Handle {
			return a.Handle < b.Handle
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.ID < b.ID
	})
	if out == nil {
		return []sessionConversation{}
	}
	return out
}

func launchConversations(sessionRoot string, agents []string) []sessionConversation {
	identity, err := fsq.SnapshotDeliveryRoot(sessionRoot)
	if err != nil {
		return nil
	}
	root, err := fsq.OpenDeliveryRoot(sessionRoot, identity)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	var out []sessionConversation
	for _, handle := range agents {
		record, err := launch.LoadConversation(root, handle)
		if err != nil || record.Identity.ID == "" {
			continue
		}
		state := conversationUncertain
		if record.State == launch.CaptureReady {
			state = conversationReady
		}
		out = append(out, sessionConversation{
			Handle: handle, Provider: record.Identity.Provider, ID: record.Identity.ID, State: state, Source: "launch",
		})
	}
	return out
}

// lineageState is the state of one record of a handle's lineage. A revoked
// record's revocation time is when the next record of the lineage was
// written, or, for the newest one, when the current marker was; records and
// markers are written once per coop exec, so their file times are those
// events. A revoked record older than the retention is dropped (ok false).
func lineageState(current, known bool, revokedAt, now time.Time) (string, bool) {
	switch {
	case !known:
		return conversationUncertain, true
	case current:
		return conversationReady, true
	case now.Sub(revokedAt) > revokedConversationRetention:
		return "", false
	default:
		return conversationRevoked, true
	}
}

// revocationTimes returns, per record index, when the record was revoked,
// given each record's lineage key, write time, and current marker time.
func revocationTimes(keys []string, recorded, marker []time.Time) []time.Time {
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return recorded[order[a]].Before(recorded[order[b]]) })
	revoked := make([]time.Time, len(keys))
	next := map[string]time.Time{}
	for i := len(order) - 1; i >= 0; i-- {
		idx := order[i]
		if t, ok := next[keys[idx]]; ok {
			revoked[idx] = t
		} else {
			revoked[idx] = marker[idx]
		}
		next[keys[idx]] = recorded[idx]
	}
	return revoked
}

func (sources conversationSources) codexConversations(rootID string) []sessionConversation {
	var entries []codexidentity.Entry
	for _, entry := range sources.codex {
		if entry.Record.RootID == rootID {
			entries = append(entries, entry)
		}
	}
	keys, recorded, marker := make([]string, len(entries)), make([]time.Time, len(entries)), make([]time.Time, len(entries))
	for i, entry := range entries {
		keys[i], recorded[i], marker[i] = entry.Record.Me, entry.RecordedAt, entry.MarkerAt
	}
	revoked := revocationTimes(keys, recorded, marker)
	var out []sessionConversation
	for i, entry := range entries {
		state, ok := lineageState(entry.Current, entry.CurrentKnown, revoked[i], sources.now)
		if !ok {
			continue
		}
		out = append(out, sessionConversation{
			Handle: entry.Record.Me, Provider: launch.CodexProvider, ID: entry.Record.Thread, State: state, Source: "codex-thread",
		})
	}
	return out
}

func (sources conversationSources) execConversations(rootID string) []sessionConversation {
	var entries []execidentity.Entry
	for _, entry := range sources.execs {
		if entry.Record.RootID == rootID {
			entries = append(entries, entry)
		}
	}
	keys, recorded, marker := make([]string, len(entries)), make([]time.Time, len(entries)), make([]time.Time, len(entries))
	for i, entry := range entries {
		keys[i], recorded[i], marker[i] = entry.Record.Me, entry.RecordedAt, entry.MarkerAt
	}
	revoked := revocationTimes(keys, recorded, marker)
	var out []sessionConversation
	for i, entry := range entries {
		state, ok := lineageState(entry.Current, entry.CurrentKnown, revoked[i], sources.now)
		if !ok {
			continue
		}
		record := entry.Record
		if record.Harness == launch.ClaudeProvider {
			if id := sources.claudeSessionID(record); id != "" {
				out = append(out, sessionConversation{Handle: record.Me, Provider: launch.ClaudeProvider, ID: id, State: state, Source: "exec-pid"})
			}
		}
		for _, b := range sources.bindings {
			if b.Mailbox() || b.NativeSession == "" || b.Harness == "" || b.Holder.PID <= 0 ||
				b.Holder.PID != record.Holder.PID || b.Holder.ProcessStart != record.Holder.ProcessStart || b.Holder.BootID != record.Holder.BootID {
				continue
			}
			out = append(out, sessionConversation{Handle: record.Me, Provider: b.Harness, ID: b.NativeSession, State: state, Source: "remote"})
		}
	}
	return out
}

// claudeSessionID joins an exec record with Claude Code's
// sessions/<pid>.json: the file must name the same pid and a process start
// at the recorded second.
func (sources conversationSources) claudeSessionID(record execidentity.Record) string {
	if sources.claudeDir == "" {
		return ""
	}
	path := filepath.Join(sources.claudeDir, "sessions", strconv.Itoa(record.Holder.PID)+".json")
	f, _, err := fsq.OpenRegularNoFollowBounded(path, 1<<20)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	var state struct {
		PID       int    `json:"pid"`
		ProcStart string `json:"procStart"`
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(f).Decode(&state); err != nil {
		return ""
	}
	start, err := time.ParseInLocation(claudeProcStartLayout, state.ProcStart, time.UTC)
	if err != nil || state.PID != record.Holder.PID || start.Unix() != record.StartUnix || !claudeSessionIDPattern.MatchString(state.SessionID) {
		return ""
	}
	return state.SessionID
}
