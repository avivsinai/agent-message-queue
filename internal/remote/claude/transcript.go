package claude

import (
	"encoding/json"
	"errors"
	"os"
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

// transcriptTailBytes is the tail window the confirmation poller reads:
// transcripts grow unboundedly, and the evidence ladder only needs the
// newest entries. 256 KiB covers many turns.
const transcriptTailBytes = 256 << 10

// readTranscriptTail returns the last transcriptTailBytes of the target's
// transcript, cut to complete lines. The open goes through openRegular
// (lstat gate, O_NOFOLLOW|O_NONBLOCK, fstat recheck) so a FIFO or symlink
// at the leaf — even one swapped in after the gate — is refused instead of
// followed or blocked on (codex #855 r1 item 9). A missing transcript
// reads as an empty tail (the harness may not have flushed anything yet).
// All failures are non-fatal: the ladder only moves forward on positive
// observation.
func readTranscriptTail(home, cwd, sessionID string) string {
	f, fi, err := openRegular(transcriptPath(home, cwd, sessionID), 0)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	size := fi.Size()
	if size > transcriptTailBytes {
		if _, err := f.Seek(size-transcriptTailBytes, 0); err != nil {
			return ""
		}
	}
	buf := make([]byte, transcriptTailBytes)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, os.ErrClosed) {
		if n <= 0 {
			return ""
		}
	}
	out := string(buf[:n])
	// The window may start mid-line (seek landed inside an entry) and end
	// mid-line (append racing the read): keep only complete JSONL entries.
	if size > transcriptTailBytes {
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		} else {
			return ""
		}
	}
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	} else {
		return "" // no complete line in the window
	}
	return out
}

// transcriptEntry is the decoded subset of one transcript JSONL line the
// ladder correlates on.
type transcriptEntry struct {
	// Type is the entry type. A peer frame absorbed mid-turn (an
	// "attachment" entry of type "queued_command") is reported as "user":
	// both are the harness accepting the frame into the conversation.
	Type string
	// Text is the content string, or the joined text blocks of a block
	// array; for an absorbed frame, the queued prompt.
	Text string
	// MsgID is origin.msg_id of a peer-delivered entry: the msg_id of the
	// frame we wrote. It is the ONLY correlation key the ladder uses.
	MsgID string
	// TS is the entry's "timestamp" in unix milliseconds, 0 when absent or
	// unparseable. The Stop binding compares marker times against it.
	TS int64
}

// peerOrigin is the harness's provenance record on a delivered frame.
type peerOrigin struct {
	Kind  string `json:"kind"`
	MsgID string `json:"msg_id"`
}

// parseTranscriptTail decodes complete JSONL lines into entries, in file
// order. Correlation happens on decoded fields, never on the raw tail:
// valid JSONL escapes the envelope's newlines and quotes, so a raw
// strings.Contains of the multi-line envelope never matches a real
// transcript (codex #855 r1 item 2). The harness records the frame's
// msg_id as origin.msg_id in both delivery shapes, observed live on
// v2.1.278: an idle target writes a "user" entry with a top-level origin
// (611.2 probe transcript), a busy target absorbs the frame mid-turn as an
// "attachment" of type "queued_command" with attachment.origin (live smoke
// 2026-09-22, which a user-entry-only reader never saw). Undecodable lines
// are skipped; the ladder only moves forward on positive observation.
func parseTranscriptTail(tail string) []transcriptEntry {
	if tail == "" {
		return nil
	}
	lines := strings.Split(tail, "\n")
	out := make([]transcriptEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var raw struct {
			Type       string      `json:"type"`
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
			continue
		}
		e := transcriptEntry{Type: raw.Type, Text: decodeContent(raw.Message.Content)}
		origin := raw.Origin
		if raw.Type == "attachment" && raw.Attachment != nil && raw.Attachment.Type == "queued_command" {
			e.Type, e.Text, origin = "user", raw.Attachment.Prompt, raw.Attachment.Origin
		}
		if origin != nil && origin.Kind == "peer" {
			e.MsgID = origin.MsgID
		}
		if ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp); err == nil {
			e.TS = ts.UnixMilli()
		}
		out = append(out, e)
	}
	return out
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
