//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// Esc or a deny on a Claude Code dialog runs no hook, but Claude Code
// 2.1.310 writes it to the session transcript at once (observed live): a
// user record with a tool_result for the asked tool call (is_error, "User
// rejected tool use"). The wake reads the transcript read-only as proof that
// the dialog of that tool_use_id is gone; without that record the hold
// stays.
//
// A PermissionRequest names no tool call. Its marker keeps the tool name
// and a digest of the tool input, only to find its call in the transcript:
// the wake binds the marker to a tool_use id only when exactly one call of
// the main conversation with that name and input was pending when the
// marker opened, and no other equal call is pending. An equal call that
// Claude Code allows without asking, or a subagent's equal call, is not
// the dialog's call, so a doubt leaves the marker unbound and the hold
// stays until a turn or session event.

// maxWakeDialogTranscriptTail bounds each transcript read; the records that
// prove a closed dialog are the newest ones.
const maxWakeDialogTranscriptTail = 256 << 10

// wakeDialogPermissionEvent is the hook event of a permission dialog.
const wakeDialogPermissionEvent = "PermissionRequest"

// claudeTranscriptRecord is the part of a transcript line the wake reads.
// Claude Code 2.1.310 writes a subagent's records to their own file with
// isSidechain true and an agentId; the main conversation's records carry
// isSidechain false and no agentId.
type claudeTranscriptRecord struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	AgentID     string `json:"agentId"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeTranscriptBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// claudeTranscriptBlocks returns the content blocks of an assistant or user
// record; other record types (api-request-blob repeats tool blocks without
// a timestamp) and a typed prompt (a string) have none.
func claudeTranscriptBlocks(line []byte) (claudeTranscriptRecord, []claudeTranscriptBlock) {
	var record claudeTranscriptRecord
	if json.Unmarshal(line, &record) != nil || (record.Type != "user" && record.Type != "assistant") {
		return record, nil
	}
	var blocks []claudeTranscriptBlock
	if json.Unmarshal(record.Message.Content, &blocks) != nil {
		return record, nil
	}
	return record, blocks
}

// markerToolUseID is the tool call whose result closes the marker: the
// PreToolUse id of an AskUserQuestion or ExitPlanMode marker, or the id the
// wake bound a permission marker to.
func markerToolUseID(marker wakeDialogMarker) string {
	if marker.ToolUseID != "" {
		return marker.ToolUseID
	}
	return marker.BoundToolUseID
}

// claudeDialogAnswered reports whether the transcript the marker names
// holds a tool_result for the marker's tool call. A marker without one is
// never proven answered. An error means no proof: the hold stays.
func claudeDialogAnswered(marker wakeDialogMarker) (bool, error) {
	id := markerToolUseID(marker)
	if id == "" {
		return false, nil
	}
	tail, _, err := readClaudeTranscriptTail(marker.TranscriptPath)
	if err != nil {
		return false, err
	}
	for line := range bytes.SplitSeq(tail, []byte{'\n'}) {
		record, blocks := claudeTranscriptBlocks(line)
		if record.Type != "user" {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_result" && block.ToolUseID == id {
				return true, nil
			}
		}
	}
	return false, nil
}

// claudePermissionCall finds the one tool call an unbound permission
// marker's dialog stands for. id is set only when exactly one main
// conversation tool_use with the marker's tool name and input digest was
// pending at the marker's open time (stamped at or before it, with no
// tool_result stamped at or before it) and no other equal call is pending
// at that time or now. Otherwise why says, in fixed text, why the marker
// stays unbound. err is a transcript that cannot be read.
func claudePermissionCall(marker wakeDialogMarker) (id, why string, err error) {
	switch {
	case marker.Agent != "":
		return "", "subagent call", nil
	case marker.InputDigest == "" || marker.Tool == "":
		return "", "no tool input digest", nil
	}
	opened, perr := time.Parse(time.RFC3339Nano, marker.OpenedAt)
	if perr != nil {
		return "", "no open time", nil
	}
	tail, cut, err := readClaudeTranscriptTail(marker.TranscriptPath)
	if err != nil {
		return "", "", err
	}
	type call struct {
		id      string
		at      time.Time
		stamped bool
		side    bool
	}
	var calls []call
	seen := make(map[string]bool)
	results := make(map[string]time.Time) // the earliest stamp; zero if unstamped
	var first time.Time
	for line := range bytes.SplitSeq(tail, []byte{'\n'}) {
		record, blocks := claudeTranscriptBlocks(line)
		at, terr := time.Parse(time.RFC3339Nano, record.Timestamp)
		stamped := terr == nil
		if stamped && first.IsZero() {
			first = at
		}
		for _, block := range blocks {
			switch {
			case record.Type == "assistant" && block.Type == "tool_use" && block.Name == marker.Tool &&
				block.ID != "" && !seen[block.ID]:
				if digest, ok := claude.ActionHash(block.Name, block.Input); ok && digest == marker.InputDigest {
					seen[block.ID] = true
					calls = append(calls, call{block.ID, at, stamped, record.IsSidechain || record.AgentID != ""})
				}
			case record.Type == "user" && block.Type == "tool_result" && block.ToolUseID != "":
				// at is zero when the record has no stamp.
				if prev, ok := results[block.ToolUseID]; !ok || (stamped && !prev.IsZero() && at.Before(prev)) {
					results[block.ToolUseID] = at
				}
			}
		}
	}
	var pending []call
	for _, c := range calls {
		result, answered := results[c.id]
		pendingNow := !answered
		// An unstamped result has no time, so it cannot show the call was
		// still pending when the marker opened.
		pendingAtOpen := c.stamped && !c.at.After(opened) && (!answered || (!result.IsZero() && result.After(opened)))
		if !pendingNow && !pendingAtOpen {
			continue
		}
		if c.side {
			return "", "ambiguous: a subagent call matches", nil
		}
		pending = append(pending, c)
	}
	switch {
	case len(pending) == 0:
		return "", "no pending call in the tail", nil
	case len(pending) > 1:
		return "", fmt.Sprintf("ambiguous: %d pending calls", len(pending)), nil
	}
	only := pending[0]
	if !only.stamped || only.at.After(opened) {
		return "", "no call pending when the dialog opened", nil
	}
	// A cut tail must reach back past the call, or an older equal call
	// could hide before it.
	if cut && (first.IsZero() || !first.Before(only.at)) {
		return "", "transcript tail starts at the call", nil
	}
	return only.id, "", nil
}

// readClaudeTranscriptTail reads at most the last
// maxWakeDialogTranscriptTail bytes of the regular file at path, without
// following a symlink, and drops a leading partial line. cut reports that
// the file holds more than the tail.
func readClaudeTranscriptTail(path string) (tail []byte, cut bool, err error) {
	if !filepath.IsAbs(path) {
		return nil, false, fmt.Errorf("transcript path %q is not absolute", path)
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat transcript: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("transcript %s is not a regular file", path)
	}
	offset := max(info.Size()-maxWakeDialogTranscriptTail, 0)
	tail = make([]byte, info.Size()-offset)
	n, err := file.ReadAt(tail, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("read transcript: %w", err)
	}
	tail = tail[:n]
	if offset > 0 {
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		} else {
			tail = nil
		}
	}
	return tail, offset > 0, nil
}

// claudeDialogProof caches a positive transcript proof per marker version
// (its bytes and mtime): the proof records may later scroll out of the
// tail, and the wake rechecks often.
type claudeDialogProof struct {
	raw     string
	modTime time.Time
}

// answered reports whether a Claude Code marker's dialog is proven gone.
// Other harnesses' markers, Startup markers, and markers without a
// transcript or a tool call never are.
func (proof *claudeDialogProof) answered(raw []byte, modTime time.Time) bool {
	if proof.raw == string(raw) && proof.modTime.Equal(modTime) {
		return true
	}
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Harness != "" ||
		marker.Event == wakeDialogStartupEvent || marker.TranscriptPath == "" || markerToolUseID(marker) == "" {
		return false
	}
	if ok, err := claudeDialogAnswered(marker); err != nil || !ok {
		return false
	}
	proof.raw, proof.modTime = string(raw), modTime
	return true
}

// unboundClaudePermissionMarker parses raw as a Claude Code permission
// marker that names a transcript and has no bound call yet.
func unboundClaudePermissionMarker(raw []byte) (wakeDialogMarker, bool) {
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Harness != "" || marker.Event != wakeDialogPermissionEvent ||
		marker.ToolUseID != "" || marker.BoundToolUseID != "" || marker.TranscriptPath == "" {
		return wakeDialogMarker{}, false
	}
	return marker, true
}
