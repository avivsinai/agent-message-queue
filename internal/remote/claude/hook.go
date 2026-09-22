package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Stop-hook bridge (bead 611.12 PR2): the hook is how the adapter learns a
// submitted turn ENDED — the transcript user line proves the payload was
// accepted and the assistant line proves the turn started, but only the
// Stop event carries the completion signal. The installer writes the hook
// into the TARGET's ~/.claude/settings.json; the receiver subcommand is
// fail-open by contract (exit 0 on ANY error) so a broken bridge can never
// block Claude Code — Claude Code's blocking-error code is exit 2.

// stopHookMarker is the settings.json ownership signal: every entry the
// installer writes carries this env assignment in the hook command, and
// uninstall removes exactly the entries that carry it.
const stopHookMarker = "AMQ_STOP_HOOK_BIN="

// stopHookType is the hook event name in settings.json.
const stopHookType = "Stop"

// settings.json is edited by BYTE SURGERY, never by decode/re-marshal:
// unrelated content (key order, whitespace, and integer literals beyond
// float64 precision — the 9007199254740993 (2^53) fixture class) must
// survive install AND uninstall unchanged, which a map round-trip cannot
// promise. The decoder below is used only to LOCATE byte ranges; the
// ranges are cut from the raw file.

// InstallStopHook adds the fail-open receiver to the Stop hook chain in
// home/.claude/settings.json. Idempotent: an entry carrying the marker
// left untouched. The settings leaf must be a regular file (lstat rule —
// a directory or symlink is refused); the write is fsq atomic-replace.
func InstallStopHook(home, bin string) error {
	return mutateStopHook(home, true, bin)
}

// UninstallStopHook removes marked entries from the Stop chain. When no
// marked entry exists the file is untouched (no rewrite, no mtime churn).
func UninstallStopHook(home string) error {
	return mutateStopHook(home, false, "")
}

func settingsPath(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

func mutateStopHook(home string, install bool, bin string) error {
	path := settingsPath(home)
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("settings %s: leaf is not a regular file; refusing", path)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		raw = []byte("{}")
	}
	var out []byte
	if install {
		entry := stopHookEntry(bin)
		out, err = rawInsertStopEntry(raw, entry)
	} else {
		out, err = rawRemoveStopEntries(raw)
	}
	if err != nil {
		return err
	}
	if out == nil {
		return nil // nothing changed
	}
	if _, err := fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), out, 0o600); err != nil {
		return err
	}
	return nil
}

func stopHookEntry(bin string) []byte {
	cmd := stopHookMarker + bin + " claude stop-hook"
	b, _ := json.Marshal(map[string]any{"type": "command", "command": cmd, "timeout": 10})
	return b
}

// locateResult carries where the Stop chain lives in the raw bytes.
type locateResult struct {
	// open/close bracket indexes of the Stop array (found case).
	open, close int64
	// hooksOpen is the index of the "hooks" object's '{' (no-Stop case).
	hooksOpen int64
	// state: 0 found, 1 hooks exists w/o Stop, 2 no hooks key at all.
	state int
	// stopNotArray: hooks.Stop exists but is not an array (edit refused).
	stopNotArray bool
}

// locateStopArray walks the decoder token stream to find byte offsets;
// numbers are decoded as json.Number so nothing here can lose precision.
func locateStopArray(raw []byte) (locateResult, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var res locateResult
	res.state = 2
	// stack of containers; lastKey tracks the pending object key.
	type frame struct {
		isObj   bool
		lastKey string
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("parse settings: %w", err)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				isObj := t == '{'
				inHooks := len(stack) > 0 && stack[len(stack)-1].lastKey == "hooks"
				if isObj && inHooks {
					res.hooksOpen = dec.InputOffset() - 1
				}
				stack = append(stack, frame{isObj: isObj})
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
		case string:
			if len(stack) > 0 && stack[len(stack)-1].isObj {
				stack[len(stack)-1].lastKey = t
				// Is this the "Stop" key inside the hooks object?
				if t == stopHookType && len(stack) >= 2 && stack[len(stack)-2].lastKey == "hooks" {
					// Next token must be '[' for the found case.
					tok2, err := dec.Token()
					if err != nil {
						return res, fmt.Errorf("parse settings: %w", err)
					}
					if d, ok := tok2.(json.Delim); ok && d == '[' {
						res.open = dec.InputOffset() - 1
						// Find the matching close bracket by walking.
						depth := 1
						for depth > 0 {
							t3, err := dec.Token()
							if err != nil {
								return res, fmt.Errorf("parse settings: %w", err)
							}
							if d3, ok := t3.(json.Delim); ok {
								if d3 == '[' || d3 == '{' {
									depth++
								} else {
									depth--
									if depth == 0 {
										res.close = dec.InputOffset()
										res.state = 0
									}
								}
							}
						}
						return res, nil
					}
					// "Stop" present but not an array: leave state 2 (no
					// usable chain); install must not clobber it.
					res.stopNotArray = true
					return res, nil
				}
			}
		}
	}
	return res, nil
}

// rawInsertStopEntry inserts the entry into the Stop chain by byte
// surgery; returns nil,nil when the marker entry already exists.
func rawInsertStopEntry(raw, entry []byte) ([]byte, error) {
	loc, err := locateStopArray(raw)
	if err != nil {
		return nil, err
	}
	if loc.stopNotArray {
		return nil, fmt.Errorf("settings: hooks.Stop is not an array; refusing to edit")
	}
	switch loc.state {
	case 0: // array exists: insert after '[' (first position)
		if bytes.Contains(raw[loc.open:loc.close], []byte(stopHookMarker)) {
			return nil, nil // already installed
		}
		insertAt := int(loc.open + 1)
		var b bytes.Buffer
		b.Write(raw[:insertAt])
		b.Write(entry)
		inner := bytes.TrimSpace(raw[loc.open+1 : loc.close])
		if len(inner) > 0 {
			b.WriteByte(',')
		}
		b.Write(raw[insertAt:])
		return b.Bytes(), nil
	case 1: // hooks object exists without a Stop chain: add the chain
		var b bytes.Buffer
		b.Write(raw[:loc.hooksOpen+1])
		b.WriteString(`"Stop": [`)
		b.Write(entry)
		b.WriteString(`],`)
		b.Write(raw[loc.hooksOpen+1:])
		return b.Bytes(), nil
	default: // no hooks key: add hooks before the final closing brace
		trimmed := bytes.TrimRight(raw, " \t\r\n")
		if bytes.Equal(bytes.TrimSpace(trimmed), []byte("{}")) {
			var b bytes.Buffer
			b.WriteString("{\n  \"hooks\": {\"" + stopHookType + "\": [")
			b.Write(entry)
			b.WriteString("]}\n}")
			return b.Bytes(), nil
		}
		last := bytes.LastIndexByte(trimmed, '}')
		if last < 0 {
			return nil, fmt.Errorf("settings: not a JSON object")
		}
		var b bytes.Buffer
		b.Write(raw[:last])
		b.WriteString(",\n  \"hooks\": {\"" + stopHookType + "\": [")
		b.Write(entry)
		b.WriteString("]}")
		b.Write(raw[last:])
		return b.Bytes(), nil
	}
}

// rawRemoveStopEntries removes marked elements from the Stop chain by
// byte surgery. Element boundaries come from the decoder with
// json.RawMessage capture, so the removed range is exact; untouched
// regions are copied verbatim. No marked entry -> nil,nil (no write).
func rawRemoveStopEntries(raw []byte) ([]byte, error) {
	loc, err := locateStopArray(raw)
	if err != nil {
		return nil, err
	}
	if loc.state != 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw[loc.open : loc.close+1]))
	dec.UseNumber()
	// Skip the '['.
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	type span struct{ start, end int }
	var cuts []span
	for dec.More() {
		start := dec.InputOffset()
		var el json.RawMessage
		if err := dec.Decode(&el); err != nil {
			return nil, fmt.Errorf("parse settings hook entry: %w", err)
		}
		end := dec.InputOffset()
		var probe struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(el, &probe); err == nil && strings.Contains(probe.Command, stopHookMarker) {
			cuts = append(cuts, span{int(loc.open + int64(start)), int(loc.open + int64(end))})
		}
	}
	if len(cuts) == 0 {
		return nil, nil
	}
	var b bytes.Buffer
	pos := 0
	for _, c := range cuts {
		b.Write(raw[pos:c.start])
		// Swallow one adjacent comma to keep the array syntactically
		// valid: prefer the trailing comma, else the preceding one.
		j := c.end
		for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
			j++
		}
		if j < len(raw) && raw[j] == ',' {
			pos = j + 1
		} else {
			// No trailing comma: this was the last element; drop the
			// comma the buffer already wrote before it, if any.
			w := b.Bytes()
			for len(w) > 0 {
				k := len(w) - 1
				if raw[k] == ',' || w[len(w)-1] == ',' {
					break
				}
				_ = k
				break
			}
			trimmed := bytes.TrimRight(w, ",")
			// Restore the comma count: replace buffer with trimmed prefix
			// (only the region before this element could carry it).
			b = *bytes.NewBuffer(trimmed)
			pos = c.end
		}
	}
	b.Write(raw[pos:])
	return b.Bytes(), nil
}

// StopHookPayload is the subset of Claude Code's Stop-hook stdin payload
// the receiver consumes (session identity + completion).
type StopHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
}

// RunStopHookReceiver is the fail-open receiver body: it appends one
// turn-completion marker line to ~/.claude/sessions/amq-stop/<sid>.jsonl
// for the adapter's confirmation poller. EVERY error exits 0 — the hook
// must never block Claude Code (exit 2 is the harness's blocking code).
func RunStopHookReceiver(home string, stdin io.Reader, stdout io.Writer) int {
	defer func() { _ = recover() }() // fail-open, unconditionally
	var payload StopHookPayload
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return 0
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	if payload.SessionID == "" {
		return 0
	}
	marker := stopMarkerPath(home, payload.SessionID)
	line, err := json.Marshal(map[string]any{"ts": time.Now().UnixMilli(), "session_id": payload.SessionID})
	if err != nil {
		return 0
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return 0
	}
	f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return 0
	}
	_ = f.Close()
	_ = stdout
	return 0
}

func stopMarkerPath(home, sessionID string) string {
	return filepath.Join(claudeSessionsDir(home), "amq-stop", sessionID+".jsonl")
}
