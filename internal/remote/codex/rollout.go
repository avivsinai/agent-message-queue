package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// RolloutLine is the part of one rollout JSONL line that the session mirror
// reads. Codex writes a rollout file for every non-ephemeral thread:
//
//	{"timestamp":"...","type":"event_msg","payload":{"type":"task_started",...}}
//
// The parse keeps only names, ids, the user's and the assistant's text and
// the end of a turn. It has no field for a command, its arguments or
// output, a file change or a tool input, so those are never decoded.
type RolloutLine struct {
	// Type is the line type (session_meta, event_msg, response_item, ...).
	// Event is the payload type of an event_msg line.
	Type  string
	Event string
	// TS is the line's time in unix milliseconds, 0 when it has none.
	TS int64
	// TurnID is the turn an event belongs to; "" when it names none.
	TurnID string
	// Trigger is what started a task_started turn (turn_attribution).
	Trigger string
	// Error is set on a task_complete that carries an error.
	Error bool
	// LastAgentMessage is a task_complete's final assistant text.
	LastAgentMessage string
	// HistoryMode is a session_meta line's history mode.
	HistoryMode string
	// Item is the item of an item_completed event.
	Item RolloutItem
}

// RolloutItem is the part of a completed turn item the mirror reads.
type RolloutItem struct {
	// Type is the item variant: UserMessage, AgentMessage, CommandExecution,
	// FileChange, McpToolCall, ...
	Type string
	ID   string
	// Text is a UserMessage's first text input, or an AgentMessage's text.
	Text string
	// Phase is an AgentMessage's phase (commentary, final_answer).
	Phase string
	// ClientID is a UserMessage's client_id: the clientUserMessageId of the
	// request that sent it.
	ClientID string
	// Source is a CommandExecution's source (agent, user_shell, ...).
	Source string
	// Server and Tool name an McpToolCall; Tool alone a DynamicToolCall or
	// CollabAgentToolCall. Kind is an Extension item's kind.
	Server string
	Tool   string
	Kind   string
}

// ParseRolloutLine parses one rollout line; false when it is not JSON.
func ParseRolloutLine(line string) (RolloutLine, bool) {
	var raw struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type            string `json:"type"`
			TurnID          string `json:"turn_id"`
			TurnAttribution *struct {
				TurnTrigger string `json:"turn_trigger"`
			} `json:"turn_attribution"`
			Error            json.RawMessage `json:"error"`
			LastAgentMessage *string         `json:"last_agent_message"`
			HistoryMode      string          `json:"history_mode"`
			Item             *struct {
				Type     string          `json:"type"`
				ID       string          `json:"id"`
				ClientID string          `json:"client_id"`
				Content  json.RawMessage `json:"content"`
				Phase    json.RawMessage `json:"phase"`
				Source   json.RawMessage `json:"source"`
				Server   json.RawMessage `json:"server"`
				Tool     json.RawMessage `json:"tool"`
				Kind     json.RawMessage `json:"kind"`
			} `json:"item"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return RolloutLine{}, false
	}
	p := raw.Payload
	l := RolloutLine{Type: raw.Type, TurnID: p.TurnID}
	if ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp); err == nil {
		l.TS = ts.UnixMilli()
	}
	switch raw.Type {
	case "session_meta":
		l.HistoryMode = p.HistoryMode
		return l, true
	case "event_msg":
	default:
		return l, true
	}
	l.Event = p.Type
	if p.TurnAttribution != nil {
		l.Trigger = p.TurnAttribution.TurnTrigger
	}
	l.Error = len(p.Error) > 0 && string(p.Error) != "null"
	if p.LastAgentMessage != nil {
		l.LastAgentMessage = *p.LastAgentMessage
	}
	if it := p.Item; it != nil {
		l.Item = RolloutItem{Type: it.Type, ID: it.ID}
		switch it.Type {
		case "UserMessage":
			l.Item.ClientID = it.ClientID
			l.Item.Text = firstText(it.Content)
		case "AgentMessage":
			l.Item.Text = allText(it.Content)
			l.Item.Phase = rawString(it.Phase)
		case "CommandExecution":
			l.Item.Source = rawString(it.Source)
		case "McpToolCall":
			l.Item.Server, l.Item.Tool = rawString(it.Server), rawString(it.Tool)
		case "DynamicToolCall", "CollabAgentToolCall":
			l.Item.Tool = rawString(it.Tool)
		case "Extension":
			l.Item.Kind = rawString(it.Kind)
		}
	}
	return l, true
}

// textInput is one text entry of a message's content: a user input
// ("text") or an agent message part ("Text").
type textInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textInputs(raw json.RawMessage) []string {
	var parts []textInput
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		if strings.EqualFold(p.Type, "text") {
			out = append(out, p.Text)
		}
	}
	return out
}

func firstText(raw json.RawMessage) string {
	if texts := textInputs(raw); len(texts) > 0 {
		return texts[0]
	}
	return ""
}

func allText(raw json.RawMessage) string {
	return strings.TrimSpace(strings.Join(textInputs(raw), "\n"))
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// rolloutHeadMax bounds the first line read to find a rollout's history
// mode; the session_meta line carries the session's base instructions.
const rolloutHeadMax = 4 << 20

// Rollout is the current rollout file of thread under codexHome (see
// CurrentRollout), when its session_meta line names the paginated history
// mode, the one that keeps completed items. "" otherwise.
func Rollout(codexHome, thread string) string {
	if codexHome == "" {
		return ""
	}
	path, _ := CurrentRollout(filepath.Join(codexHome, "sessions"), thread)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head, err := bufio.NewReader(io.LimitReader(f, rolloutHeadMax)).ReadString('\n')
	if err != nil {
		return ""
	}
	l, ok := ParseRolloutLine(strings.TrimSpace(head))
	if !ok || l.Type != "session_meta" || l.HistoryMode != "paginated" {
		return ""
	}
	return path
}

// rolloutName is a parsed rollout file name:
// rollout-<YYYY-MM-DDThh-mm-ss>-<thread>[_<rollout id>].jsonl[.zst]. A
// thread's first file has no rollout id (it equals the thread id); a
// thread/revert moves the thread to a new file with its own rollout id.
type rolloutName struct {
	time, thread, rollout string
	compressed            bool
}

// hyphenatedUUID is the form Codex writes a thread or rollout id in a file
// name.
var hyphenatedUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseRolloutName(name string) (rolloutName, bool) {
	var n rolloutName
	name, n.compressed = strings.CutSuffix(name, ".zst")
	core, ok := strings.CutPrefix(name, "rollout-")
	if !ok {
		return n, false
	}
	if core, ok = strings.CutSuffix(core, ".jsonl"); !ok || len(core) < 20 || core[19] != '-' {
		return n, false
	}
	n.time = core[:19]
	if _, err := time.Parse("2006-01-02T15-04-05", n.time); err != nil {
		return n, false
	}
	thread, rollout, found := strings.Cut(core[20:], "_")
	if !found {
		rollout = thread
	}
	if !hyphenatedUUID.MatchString(thread) || !hyphenatedUUID.MatchString(rollout) {
		return n, false
	}
	n.thread, n.rollout = strings.ToLower(thread), strings.ToLower(rollout)
	return n, true
}

// RolloutThread is the thread id a rollout file's name carries, "" when the
// name is not a rollout file's.
func RolloutThread(path string) string {
	n, ok := parseRolloutName(filepath.Base(path))
	if !ok {
		return ""
	}
	return n.thread
}

// SessionsDir is the sessions directory that holds the rollout file path,
// written as <sessions>/YYYY/MM/DD/<file>; "" for any other path.
func SessionsDir(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	dir := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	if filepath.Base(dir) != "sessions" {
		return ""
	}
	return dir
}

// rolloutPick keeps the file Codex reads for one thread among the files it
// is shown, by Codex's own rule (find_thread_path_by_id_from_filenames in
// codex-rs/rollout/src/list.rs): the newest name time, then the greatest
// rollout id; a compressed file whose plain sibling exists is the same file.
type rolloutPick struct {
	thread string
	best   rolloutName
	path   string
	found  bool
}

func (k *rolloutPick) consider(path string, d fs.DirEntry) {
	if !d.Type().IsRegular() {
		return
	}
	n, match := parseRolloutName(d.Name())
	if !match || n.thread != k.thread {
		return
	}
	b := k.best
	if !k.found || n.time > b.time || n.time == b.time && n.rollout > b.rollout {
		k.best, k.path, k.found = n, path, true
	} else if n.time == b.time && n.rollout == b.rollout && b.compressed && !n.compressed {
		k.best, k.path = n, path
	}
}

// result is the picked plain file; "" when it exists only compressed,
// which the mirror does not read.
func (k *rolloutPick) result() (string, bool) {
	if !k.found || k.best.compressed {
		return "", k.found
	}
	return k.path, true
}

// CurrentRollout is the rollout file Codex reads for thread, of the regular
// files at any depth under sessions (see rolloutPick). It reads the whole
// sessions directory. ok is false when no file matches or the directory
// cannot be read; path is "" when the current file exists only compressed.
func CurrentRollout(sessions, thread string) (path string, ok bool) {
	if sessions == "" || !hyphenatedUUID.MatchString(thread) {
		return "", false
	}
	k := rolloutPick{thread: strings.ToLower(thread)}
	err := filepath.WalkDir(sessions, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != sessions && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		k.consider(p, d)
		return nil
	})
	if err != nil {
		return "", false
	}
	return k.result()
}

// CurrentRolloutSince is CurrentRollout read only from the date folders
// where a file newer than known can be, with known a rollout file of thread
// under sessions. Codex names a new rollout file, a thread/revert's
// included, and its sessions/YYYY/MM/DD folder by one local time of its
// creation (precompute_new_rollout_path in codex-rs/rollout/src/recorder.rs),
// so a newer file is in a folder from known's date through today. The range
// is widened by a day on each side for a time zone change. The folder of
// known itself is always read. It never reads the whole sessions directory,
// which grows with the host's history: with no valid known file, or no file
// of thread in those folders, it reports none.
func CurrentRolloutSince(sessions, thread, known string, now time.Time) (path string, ok bool) {
	n, valid := parseRolloutName(filepath.Base(known))
	if sessions == "" || !valid || n.thread != strings.ToLower(thread) || SessionsDir(known) != sessions {
		return "", false
	}
	from, err := time.Parse("2006-01-02", n.time[:10])
	if err != nil {
		return "", false
	}
	lo, hi := from.AddDate(0, 0, -1).Format("2006-01-02"), now.AddDate(0, 0, 1).Format("2006-01-02")
	k := rolloutPick{thread: n.thread}
	scan := func(dir string) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			k.consider(filepath.Join(dir, e.Name()), e)
		}
	}
	knownDir := filepath.Dir(known)
	scan(knownDir)
	for _, y := range dirNames(sessions, 4) {
		if y < lo[:4] || y > hi[:4] {
			continue
		}
		for _, m := range dirNames(filepath.Join(sessions, y), 2) {
			if ym := y + "-" + m; ym < lo[:7] || ym > hi[:7] {
				continue
			}
			for _, d := range dirNames(filepath.Join(sessions, y, m), 2) {
				dir := filepath.Join(sessions, y, m, d)
				if ymd := y + "-" + m + "-" + d; ymd < lo || ymd > hi || dir == knownDir {
					continue
				}
				scan(dir)
			}
		}
	}
	if !k.found {
		return "", false
	}
	return k.result()
}

// dirNames lists the names of dir's subdirectories that are digits of
// length width.
func dirNames(dir string, width int) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && len(name) == width && strings.Trim(name, "0123456789") == "" {
			out = append(out, name)
		}
	}
	return out
}
