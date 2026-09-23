package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// transcriptPath maps cwd+sessionId to the target's JSONL transcript:
// ~/.claude/projects/<slug>/<sessionId>.jsonl (slug = cwd with every
// non-alphanumeric character replaced by '-' — the observed rule, e.g.
// "Application Support" → "Application-Support").
func transcriptPath(home, cwd, sessionID string) string {
	return filepath.Join(home, ".claude", "projects", slugifyCwd(cwd), sessionID+".jsonl")
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
	out := transcriptRead{next: off, size: size, skipping: skipping}
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
		if int64(len(buf)) == transcriptChunkBytes {
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
	// MsgID is origin.msg_id of a peer-delivered entry: the msg_id of the
	// frame we wrote. It is the ONLY delivery-ownership key.
	MsgID string
	// TS is the entry's "timestamp" in unix milliseconds, 0 when absent or
	// unparseable. Stop markers are bound to turns by it.
	TS int64
}

// peerOrigin is the harness's provenance record on a delivered frame.
type peerOrigin struct {
	Kind  string `json:"kind"`
	MsgID string `json:"msg_id"`
}

// parseTranscriptLine decodes one JSONL line. Correlation happens on
// decoded fields, never on raw bytes: valid JSONL escapes the envelope's
// newlines and quotes (codex #855 r1 item 2). The harness records the
// frame's msg_id as origin.msg_id in both delivery shapes, observed live on
// v2.1.278: an idle target writes a "user" entry with a top-level origin
// (611.2 probe transcript); a busy target absorbs the frame mid-turn as an
// "attachment" of type "queued_command" with attachment.origin (live smoke
// 2026-09-22). ok is false for undecodable or untyped lines.
func parseTranscriptLine(line string) (transcriptEntry, bool) {
	var raw struct {
		Type       string      `json:"type"`
		IsMeta     bool        `json:"isMeta"`
		Timestamp  string      `json:"timestamp"`
		Origin     *peerOrigin `json:"origin"`
		Attachment *struct {
			Type   string      `json:"type"`
			Prompt string      `json:"prompt"`
			Origin *peerOrigin `json:"origin"`
		} `json:"attachment"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil || raw.Type == "" {
		return transcriptEntry{}, false
	}
	e := transcriptEntry{Type: raw.Type, Meta: raw.IsMeta, Text: decodeContent(raw.Message.Content)}
	origin := raw.Origin
	if raw.Type == "attachment" && raw.Attachment != nil && raw.Attachment.Type == "queued_command" {
		e.Type, e.Absorbed, e.Meta, e.Text, origin = "user", true, false, raw.Attachment.Prompt, raw.Attachment.Origin
	}
	if origin != nil && origin.Kind == "peer" {
		e.MsgID = origin.MsgID
		// A peer delivery is input, whatever its meta flag: v2.1.280 marks
		// the delivered user entry isMeta:true (observed live 2026-09-22),
		// which v2.1.278 did not, and the ladder skips meta entries.
		e.Meta = false
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
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
