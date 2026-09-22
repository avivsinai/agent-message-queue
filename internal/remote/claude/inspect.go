package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// agentRoster runs `claude agents --json` and parses the roster. The
// command is read-only (a roster listing); there is no prompt, no
// keystroke, and no print child — the binary runs with no arguments beyond
// the fixed `agents --json` and no stdin.
func agentRoster(claudeBin string) ([]rosterEntry, error) {
	if claudeBin == "" {
		claudeBin = "claude"
	}
	ctx, cancel := context.WithTimeout(context.Background(), rosterTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, claudeBin, "agents", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("claude agents --json: %w", err)
	}
	var entries []rosterEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("claude agents --json: parse roster: %w", err)
	}
	return entries, nil
}

// rosterTimeout bounds the roster poll so Inspect can never hang the
// endpoint's projection loop.
const rosterTimeout = 10 * time.Second

// transcriptTailLine is one decoded transcript JSONL line, reduced to the
// fields PR1/PR2 read. Unknown fields are ignored.
type transcriptTailLine struct {
	Type      string `json:"type"`
	PromptID  string `json:"promptId"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
}

// transcriptTail returns the last N decoded lines of the session's
// transcript JSONL. It reads only the tail (the file is append-only and
// can be very large): the reader seeks to the last window of the file and
// drops any partial first line. A missing transcript is an empty tail,
// not an error (a fresh session may have no transcript yet).
func transcriptTail(home, cwd, sessionID string, n int) ([]transcriptTailLine, error) {
	path := transcriptPath(home, cwd, sessionID)
	return readTranscriptTail(path, n)
}

const transcriptTailWindow = 256 << 10 // 256 KiB tail window

// readTranscriptTail decodes the last complete JSON objects in the file's
// tail window, newest last. A truncated trailing line (a writer mid-
// append) is dropped, never half-parsed.
func readTranscriptTail(path string, n int) ([]transcriptTailLine, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("transcript %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("transcript %s: %w", path, err)
	}
	window := int64(transcriptTailWindow)
	if st.Size() < window {
		window = st.Size()
	}
	buf := make([]byte, window)
	if _, err := f.ReadAt(buf, st.Size()-window); err != nil {
		return nil, fmt.Errorf("transcript %s: %w", path, err)
	}
	// Drop the partial first line unless the window covers the whole file.
	start := 0
	if st.Size() > window {
		if idx := bytes.IndexByte(buf, '\n'); idx >= 0 {
			start = idx + 1
		} else {
			return nil, nil
		}
	}
	// Only lines the writer fully terminated count: a trailing element
	// after the last \n is a partial append and is dropped, never half-
	// parsed. When the buffer ends with \n, Split's final empty element is
	// skipped by the TrimSpace guard below.
	segment := buf[start:]
	complete := segment
	if len(segment) == 0 || segment[len(segment)-1] != '\n' {
		if idx := bytes.LastIndexByte(segment, '\n'); idx >= 0 {
			complete = segment[:idx+1]
		} else {
			complete = nil
		}
	}
	lines := bytes.Split(complete, []byte("\n"))
	var out []transcriptTailLine
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var l transcriptTailLine
		if err := json.Unmarshal(line, &l); err != nil {
			continue // a non-JSON line is skipped, never fatal
		}
		out = append(out, l)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}
