package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// transcriptPath maps cwd+sessionId to the target's JSONL transcript:
// <config dir>/projects/<slug>/<sessionId>.jsonl (slug = cwd with every
// non-alphanumeric character replaced by '-' — the observed rule, e.g.
// "Application Support" → "Application-Support").
func transcriptPath(dir, cwd, sessionID string) string {
	return filepath.Join(dir, "projects", slugifyCwd(cwd), sessionID+".jsonl")
}

func slugifyCwd(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return b.String()
}

// transcriptChunkBytes bounds one poll's read of new transcript bytes; the
// cursor resumes where it stopped on the next tick, so a burst of large
// entries delays confirmation but never drops an entry.
const transcriptChunkBytes = 4 << 20

// transcriptRead is one bounded read of complete JSONL lines starting at a
// byte offset.
type transcriptRead struct {
	lines []string
	// starts[i] is the byte offset where lines[i] begins.
	starts []int64
	// next is the offset just past the last consumed byte: the end of the
	// last complete line, or of a discarded over-long line segment.
	next int64
	// size is the file size observed by fstat on the open description.
	size int64
	// skipping is true when the read ended inside a line longer than one
	// chunk; the caller keeps discarding until that line's newline.
	skipping bool
	// info is the fstat of the open description.
	info os.FileInfo
}

// readTranscriptFrom returns the complete lines appended at or after byte
// offset off, bounded by transcriptChunkBytes. The open goes through
// openRegular (lstat gate, no-follow non-blocking open, same-file check)
// so a FIFO or symlink at the leaf is refused rather than followed or
// blocked on. A partial trailing line is left for the next read. When
// skipping is set, bytes up to and including the next newline are
// discarded first (the tail of an over-long line). Errors are returned so
// the caller can tell "nothing new" from "cannot read".
func readTranscriptFrom(path string, off int64, skipping bool) (transcriptRead, error) {
	f, fi, err := openRegular(path, 0)
	if err != nil {
		return transcriptRead{}, err
	}
	defer func() { _ = f.Close() }()
	size := fi.Size()
	out := transcriptRead{next: off, size: size, skipping: skipping, info: fi}
	if off >= size {
		return out, nil
	}
	n := size - off
	if n > transcriptChunkBytes {
		n = transcriptChunkBytes
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return transcriptRead{}, err
	}
	buf = buf[:got]
	pos := 0
	if out.skipping {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			out.next = off + int64(len(buf))
			return out, nil
		}
		pos = i + 1
		out.skipping = false
	}
	last := bytes.LastIndexByte(buf[pos:], '\n')
	if last < 0 {
		if int64(len(buf)-pos) == transcriptChunkBytes {
			// One line longer than a whole chunk: discard it in pieces.
			out.next = off + int64(len(buf))
			out.skipping = true
			return out, nil
		}
		out.next = off + int64(pos)
		return out, nil
	}
	end := pos + last
	start := pos
	for _, line := range bytes.Split(buf[pos:end], []byte{'\n'}) {
		if len(line) > 0 {
			out.lines = append(out.lines, string(line))
			out.starts = append(out.starts, off+int64(start))
		}
		start += len(line) + 1
	}
	out.next = off + int64(end) + 1
	return out, nil
}

// TranscriptChunk is one bounded read of complete transcript lines.
type TranscriptChunk struct {
	Lines []string
	// Next is the offset the next read starts at. Skipping is set while the
	// read is inside a line longer than one chunk: the next read discards up
	// to that line's newline.
	Next     int64
	Skipping bool
	// Size is the file size; File the file's identity (device and inode),
	// "" when the platform gives none.
	Size int64
	File string
}

// ReadTranscript is the bounded read of complete lines from off that the
// adapter's own cursor makes: at most one chunk, a partial last line left
// for the next read, and a line longer than a chunk skipped without its
// bytes being kept.
func ReadTranscript(path string, off int64, skipping bool) (TranscriptChunk, error) {
	r, err := readTranscriptFrom(path, off, skipping)
	if err != nil {
		return TranscriptChunk{}, err
	}
	id, _ := fsq.StableFileIdentityInfo(r.info)
	return TranscriptChunk{Lines: r.lines, Next: r.next, Skipping: r.skipping, Size: r.size, File: id}, nil
}

// TranscriptTail is the first read of a cursor that starts at the end of
// path: the complete lines of its last chunk, with Next after the last
// complete line.
func TranscriptTail(path string) (TranscriptChunk, error) {
	r, err := readTranscriptFrom(path, math.MaxInt64, false)
	if err != nil {
		return TranscriptChunk{}, err
	}
	off, skipping := transcriptStartOffset(r.size)
	return ReadTranscript(path, off, skipping)
}

// SessionTranscript is the transcript that the registry entry of the Claude
// process pid names, read under the config dir that holds transcript (its
// third parent, <config dir>/projects/<slug>/<id>.jsonl), or, when
// transcript is "", under this process's own config dir. It follows /clear
// and /resume, which change the entry's sessionId. "" when pid has no entry
// there or the transcript it names is not a regular file.
func SessionTranscript(transcript string, pid int) string {
	if pid <= 0 {
		return ""
	}
	var dir string
	switch {
	case transcript == "":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		if dir, err = sessionConfigDir(home); err != nil {
			return ""
		}
	case filepath.IsAbs(transcript):
		dir = filepath.Dir(filepath.Dir(filepath.Dir(transcript)))
	default:
		return ""
	}
	reg, err := readSessionRegistry(dir, pid)
	if err != nil || reg == nil {
		return ""
	}
	if path := transcriptPath(dir, reg.Cwd, reg.SessionID); regularFile(path) {
		return path
	}
	return ""
}

// transcriptStartOffset is where a fresh cursor starts when no run carries
// a submit-time offset (the file did not exist at submit): the last
// transcriptChunkBytes, aligned by the caller's skipping flag.
func transcriptStartOffset(size int64) (int64, bool) {
	if size <= transcriptChunkBytes {
		return 0, false
	}
	return size - transcriptChunkBytes, true
}

// transcriptEntry is the decoded subset of one transcript JSONL line the
// ladder correlates on.
type transcriptEntry struct {
	// Type is the entry type. A frame absorbed mid-turn (an "attachment"
	// entry of type "queued_command") is reported as "user" with Absorbed
	// set: both are the harness accepting input into the conversation.
	Type string
	// Absorbed marks input folded into the turn already running, as
	// opposed to a user entry that starts a new turn.
	Absorbed bool
	// Meta marks harness-injected user entries (isMeta: caveats, command
	// output). They are not prompts and never start or end a turn. A peer
	// delivery (MsgID set) is never Meta.
	Meta bool
	// Text is the content string, or the joined text blocks of a block
	// array; for an absorbed frame, the queued prompt. Tool results decode
	// to "".
	Text string
	// Blocks are the content blocks the line actually carried, except that
	// a peer delivery's text is the envelope body. Text stays the joined
	// text blocks so the ladder's correlation is unchanged.
	Blocks []TranscriptBlock
	// SessionID and UUID are the line's own ids when present. They are not
	// paths.
	SessionID string
	UUID      string
	// MsgID is origin.msg_id of a peer-delivered entry: the msg_id of the
	// frame we wrote. It is the ONLY delivery-ownership key.
	MsgID string
	// TS is the entry's "timestamp" in unix milliseconds, 0 when absent or
	// unparseable. Stop markers are bound to turns by it.
	TS int64
	// PromptID is the line's promptId: the prompt a user line starts, or
	// the prompt a tool_result belongs to. The PermissionRequest hook gets
	// the same id as prompt_id. Absorbed frames and assistant lines carry
	// none.
	PromptID string
	// ToolUses are the line's tool_use blocks with their input hash;
	// ToolResults the tool_use ids its tool_result blocks answer.
	ToolUses    []toolUseRef
	ToolResults []string
	// OriginKind is the harness's origin.kind for the input (human, peer,
	// task-notification, auto-continuation); "" when the line has none.
	OriginKind string
	// TurnOrigin is turnOrigin: "scheduled" on the prompt of a /loop or
	// cron fire.
	TurnOrigin string
	// CrossSession marks text that holds a complete cross-session envelope,
	// with or without an origin record.
	CrossSession bool
	// Sidechain, CompactSummary and VisibleOnly are isSidechain,
	// isCompactSummary and isVisibleInTranscriptOnly.
	Sidechain, CompactSummary, VisibleOnly bool
	// MessageID and StopReason are message.id and message.stop_reason of an
	// assistant line. Claude writes one line per content block of a message,
	// each with the same id and stop reason.
	MessageID, StopReason string
	// APIError is isApiErrorMessage: a synthetic assistant line for a
	// failed API call.
	APIError bool
	// Model is message.model; "<synthetic>" on a line Claude Code writes
	// itself.
	Model string
}

// toolUseRef is one tool_use block: its id, tool name and actionHash of
// its input.
type toolUseRef struct {
	ID, Name, Hash string
}

// peerOrigin is the harness's provenance record on a delivered frame.
type peerOrigin struct {
	Kind  string `json:"kind"`
	MsgID string `json:"msg_id"`
}

// parseTranscriptLine decodes one JSONL line. Correlation happens on
// decoded fields, never on raw bytes: valid JSONL escapes the envelope's
// newlines and quotes. The harness records the frame's msg_id as
// origin.msg_id in both delivery shapes, observed live on
// v2.1.278: an idle target writes a "user" entry with a top-level origin;
// a busy target absorbs the frame mid-turn as an "attachment" of type
// "queued_command" with attachment.origin. ok is false for undecodable or
// untyped lines.
func parseTranscriptLine(line string) (transcriptEntry, bool) {
	var raw struct {
		SessionID     string      `json:"sessionId"`
		UUID          string      `json:"uuid"`
		Type          string      `json:"type"`
		IsMeta        bool        `json:"isMeta"`
		IsSidechain   bool        `json:"isSidechain"`
		IsCompact     bool        `json:"isCompactSummary"`
		IsVisibleOnly bool        `json:"isVisibleInTranscriptOnly"`
		IsAPIError    bool        `json:"isApiErrorMessage"`
		Timestamp     string      `json:"timestamp"`
		PromptID      string      `json:"promptId"`
		TurnOrigin    string      `json:"turnOrigin"`
		Origin        *peerOrigin `json:"origin"`
		Attachment    *struct {
			Type   string      `json:"type"`
			Prompt string      `json:"prompt"`
			Origin *peerOrigin `json:"origin"`
		} `json:"attachment"`
		Message struct {
			ID         string          `json:"id"`
			Model      string          `json:"model"`
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil || raw.Type == "" {
		return transcriptEntry{}, false
	}
	text, blocks, uses := decodeContentWalk(raw.Message.Content)
	e := transcriptEntry{Type: raw.Type, Meta: raw.IsMeta, Text: text, Blocks: blocks, SessionID: raw.SessionID, UUID: raw.UUID, PromptID: raw.PromptID, ToolUses: uses,
		TurnOrigin: raw.TurnOrigin, Sidechain: raw.IsSidechain, CompactSummary: raw.IsCompact, VisibleOnly: raw.IsVisibleOnly,
		MessageID: raw.Message.ID, StopReason: raw.Message.StopReason, APIError: raw.IsAPIError, Model: raw.Message.Model}
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ID != "" {
			e.ToolResults = append(e.ToolResults, b.ID)
		}
	}
	origin := raw.Origin
	if raw.Type == "attachment" && raw.Attachment != nil && raw.Attachment.Type == "queued_command" {
		e.Type, e.Absorbed, e.Meta, e.Text, origin = "user", true, false, raw.Attachment.Prompt, raw.Attachment.Origin
		e.Blocks, e.ToolUses, e.ToolResults, e.PromptID = nil, nil, nil, ""
		if e.Text != "" {
			e.Blocks = []TranscriptBlock{{Type: "text", Text: e.Text}}
		}
	}
	if origin != nil {
		e.OriginKind = origin.Kind
	}
	body, cross := crossSessionBodies(e.Text)
	e.CrossSession = cross
	if origin != nil && origin.Kind == "peer" {
		e.MsgID = origin.MsgID
		// A peer delivery is input, whatever its meta flag: v2.1.280 marks
		// the delivered user entry isMeta:true (observed live),
		// which v2.1.278 did not, and the ladder skips meta entries.
		e.Meta = false
		// The activity view shows the prompt the sender wrote, not the
		// envelope and harness prose. Text keeps the raw content.
		if cross {
			blocks := []TranscriptBlock{{Type: "text", Text: body}}
			for _, b := range e.Blocks {
				if b.Type != "text" {
					blocks = append(blocks, b)
				}
			}
			e.Blocks = blocks
		}
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp); err == nil {
		e.TS = ts.UnixMilli()
	}
	return e, true
}

// decodeContent renders a transcript message content field as text: a
// JSON string as-is, a block array as its text blocks joined by newlines.
// Anything else (tool results, unknown shapes) yields "".
func decodeContent(raw json.RawMessage) string {
	text, _ := decodeContentBlocks(raw)
	return text
}

// TranscriptBlock is one content block the transcript parse already walked.
// Tool input is omitted: it carries local paths.
type TranscriptBlock struct {
	Type   string
	Text   string
	Name   string
	ID     string
	Failed bool
}

// decodeContentBlocks is the one content walk. Text blocks feed the ladder.
// tool_use and tool_result are kept beside that text, and tool_use input is
// not copied: it carries local paths.
func decodeContentBlocks(raw json.RawMessage) (string, []TranscriptBlock) {
	text, blocks, _ := decodeContentWalk(raw)
	return text, blocks
}

// decodeContentWalk is decodeContentBlocks plus each tool_use block's input
// hash, which identifies the call a permission dialog is about. The input itself is
// hashed, never kept.
func decodeContentWalk(raw json.RawMessage) (string, []TranscriptBlock, []toolUseRef) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return "", nil, nil
		}
		return s, []TranscriptBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`
		ID        string          `json:"id"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
		Input     json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, nil
	}
	var parts []string
	var out []TranscriptBlock
	var uses []toolUseRef
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			parts = append(parts, b.Text)
			out = append(out, TranscriptBlock{Type: "text", Text: b.Text})
		case "tool_use":
			if b.Name == "" && b.ID == "" {
				continue
			}
			out = append(out, TranscriptBlock{Type: "tool_use", Name: b.Name, ID: b.ID})
			if h, ok := actionHash(b.Name, b.Input); ok && b.ID != "" {
				uses = append(uses, toolUseRef{ID: b.ID, Name: b.Name, Hash: h})
			}
		case "tool_result":
			out = append(out, TranscriptBlock{Type: "tool_result", ID: b.ToolUseID, Text: decodeContent(b.Content), Failed: b.IsError})
		}
	}
	return strings.Join(parts, "\n"), out, uses
}

// TranscriptLine is the activity and mirror view of one parsed transcript
// line. An absorbed frame (Absorbed) has Type "user" and no PromptID.
type TranscriptLine struct {
	Type      string
	Meta      bool
	SessionID string
	UUID      string
	TS        int64
	Blocks    []TranscriptBlock
	// PromptID is the user line's promptId; MsgID the origin.msg_id of a
	// peer delivery; OriginKind its origin.kind; TurnOrigin its turnOrigin.
	PromptID, MsgID, OriginKind, TurnOrigin string
	// Absorbed marks input folded into the running turn (a queued_command
	// attachment).
	Absorbed bool
	// CrossSession marks text that holds a cross-session envelope.
	CrossSession bool
	// Sidechain, CompactSummary and VisibleOnly are the line's
	// isSidechain, isCompactSummary and isVisibleInTranscriptOnly.
	Sidechain, CompactSummary, VisibleOnly bool
	// MessageID, StopReason and APIError are an assistant line's message.id,
	// message.stop_reason and isApiErrorMessage.
	MessageID, StopReason string
	APIError              bool
}

// InterruptPrefix opens the user line Claude Code writes when the user
// interrupts a turn, plain or "for tool use".
const InterruptPrefix = interruptPrefix

// line is the exported view of e.
func (e transcriptEntry) line() TranscriptLine {
	return TranscriptLine{
		Type: e.Type, Meta: e.Meta, SessionID: e.SessionID, UUID: e.UUID, TS: e.TS,
		Blocks: e.Blocks, PromptID: e.PromptID, MsgID: e.MsgID, OriginKind: e.OriginKind, TurnOrigin: e.TurnOrigin,
		Absorbed: e.Absorbed, CrossSession: e.CrossSession, Sidechain: e.Sidechain,
		CompactSummary: e.CompactSummary, VisibleOnly: e.VisibleOnly,
		MessageID: e.MessageID, StopReason: e.StopReason, APIError: e.APIError,
	}
}

// ActivityNote is one parsed transcript line plus the cursor's session and
// the turn opened by a user or absorbed boundary. Line.UUID is the message
// identity, not the turn.
type ActivityNote struct {
	Line      TranscriptLine
	SessionID string
	TurnID    string
}

// ParseTranscriptLine is the exported form of the adapter's one transcript
// parse. Callers must not decode the JSONL a second way.
func ParseTranscriptLine(line string) (TranscriptLine, bool) {
	e, ok := parseTranscriptLine(line)
	if !ok {
		return TranscriptLine{}, false
	}
	return e.line(), true
}

// transcriptNameRe is a session or subagent id that names exactly one
// path component.
var transcriptNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// SubagentTranscript is the transcript of subagent agentID in session
// sessionID, at the path Claude Code lays out beside the session transcript
// <dir>/<session_id>.jsonl:
// <dir>/<session_id>/subagents/agent-<agent_id>.jsonl, or one or two
// subdirectories deeper (a workflow's subagents write to
// subagents/workflows/<wf>/). Invariant: the proof reads only a regular
// file, reached through no symlink below <dir>, named for exactly this
// agent_id, and found exactly once across the three depths; anything else
// is an error and no proof.
func SubagentTranscript(sessionTranscript, sessionID, agentID string) (string, error) {
	if !transcriptNameRe.MatchString(agentID) || !transcriptNameRe.MatchString(sessionID) ||
		!filepath.IsAbs(sessionTranscript) || filepath.Base(sessionTranscript) != sessionID+".jsonl" {
		return "", errors.New("no subagent transcript path for this session")
	}
	session := filepath.Join(filepath.Dir(sessionTranscript), sessionID)
	subagents := filepath.Join(session, "subagents")
	for _, dir := range []string{session, subagents} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return "", fmt.Errorf("subagent transcripts: %s is not a directory", dir)
		}
	}
	name := "agent-" + agentID + ".jsonl"
	regular := func(path string) bool {
		info, err := os.Lstat(path)
		return err == nil && info.Mode().IsRegular()
	}
	var found []string
	var search func(dir string, depth int) error
	search = func(dir string, depth int) error {
		if path := filepath.Join(dir, name); regular(path) {
			found = append(found, path)
		}
		if depth == 2 {
			return nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read subagent transcripts: %w", err)
		}
		for _, entry := range entries {
			// A symlink is not a directory here, so the search follows none.
			if entry.IsDir() {
				if err := search(filepath.Join(dir, entry.Name()), depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := search(subagents, 0); err != nil {
		return "", err
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%d subagent transcripts named %s", len(found), name)
	}
	return found[0], nil
}
