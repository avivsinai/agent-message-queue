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
)

// Esc or a deny on a Claude Code AskUserQuestion or ExitPlanMode dialog
// runs no hook, but Claude Code 2.1.310 writes it to the session transcript
// at once (observed live): a user record with a tool_result for the asked
// tool call (is_error, "User rejected tool use"). The wake reads the
// transcript read-only as proof that the dialog of that tool_use_id is
// gone; without that record the hold stays.
//
// A permission marker has no tool_use_id. While a permission dialog blocks a
// tool call, the main chain writes no new assistant message: the model waits
// for the whole tool batch. A main-chain assistant message that starts after
// the marker opened therefore proves the session moved past the dialog.
// Sidechain (subagent) records run alongside a dialog and prove nothing, and
// a later record of an assistant message already started when the marker
// opened (the batch that asked) is not a new message.

// maxWakeDialogTranscriptTail bounds each transcript read; the records that
// prove a closed dialog are the newest ones.
const maxWakeDialogTranscriptTail = 256 << 10

type claudeTranscriptRecord struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Timestamp   string `json:"timestamp"`
	Message     struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeTranscriptBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
}

// claudeDialogAnswered reports whether the transcript the marker names
// proves its dialog gone: a tool_result for the marker's tool_use_id, or for
// a permission marker (claudePermissionMarker) a new main-chain assistant
// message after it opened (claudeTurnMovedOn). Any other marker is never
// proven answered. An error means no proof: the hold stays.
func claudeDialogAnswered(marker wakeDialogMarker, modTime time.Time) (bool, error) {
	permission := claudePermissionMarker(marker)
	if marker.ToolUseID == "" && !permission {
		return false, nil
	}
	tail, err := readClaudeTranscriptTail(marker.TranscriptPath)
	if err != nil {
		return false, err
	}
	if permission {
		return claudeTurnMovedOn(tail, wakeDialogMarkerOpened(marker, modTime)), nil
	}
	for line := range bytes.SplitSeq(tail, []byte{'\n'}) {
		var record claudeTranscriptRecord
		if json.Unmarshal(line, &record) != nil || record.Type != "user" {
			continue
		}
		var blocks []claudeTranscriptBlock
		if json.Unmarshal(record.Message.Content, &blocks) != nil {
			continue // a typed prompt is a string
		}
		for _, block := range blocks {
			if block.Type == "tool_result" && block.ToolUseID == marker.ToolUseID {
				return true, nil
			}
		}
	}
	return false, nil
}

// claudePermissionMarker reports a Claude Code permission marker: event
// PermissionRequest, no harness, and no tool_use_id.
func claudePermissionMarker(marker wakeDialogMarker) bool {
	return marker.Event == "PermissionRequest" && marker.Harness == "" && marker.ToolUseID == ""
}

// wakeDialogMarkerOpened is the latest time the marker's dialog can have
// opened: the end of its OpenedAt second (OpenedAt has whole seconds) or
// its mtime, whichever is later. A later bound only delays a proof.
func wakeDialogMarkerOpened(marker wakeDialogMarker, modTime time.Time) time.Time {
	opened := modTime
	if at, err := time.Parse(time.RFC3339Nano, marker.OpenedAt); err == nil {
		if end := at.Truncate(time.Second).Add(time.Second); end.After(opened) {
			opened = end
		}
	}
	return opened
}

// claudeTurnMovedOn reports whether tail holds a main-chain assistant
// record dated after opened whose message has no record dated at or before
// opened.
func claudeTurnMovedOn(tail []byte, opened time.Time) bool {
	before := make(map[string]bool)
	var after []string
	for line := range bytes.SplitSeq(tail, []byte{'\n'}) {
		var record claudeTranscriptRecord
		if json.Unmarshal(line, &record) != nil || record.Type != "assistant" || record.IsSidechain || record.Message.ID == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, record.Timestamp)
		if err != nil {
			continue
		}
		if at.After(opened) {
			after = append(after, record.Message.ID)
		} else {
			before[record.Message.ID] = true
		}
	}
	for _, id := range after {
		if !before[id] {
			return true
		}
	}
	return false
}

// readClaudeTranscriptTail reads at most the last
// maxWakeDialogTranscriptTail bytes of the regular file at path, without
// following a symlink, and drops a leading partial line.
func readClaudeTranscriptTail(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("transcript path %q is not absolute", path)
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat transcript: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("transcript %s is not a regular file", path)
	}
	offset := max(info.Size()-maxWakeDialogTranscriptTail, 0)
	tail := make([]byte, info.Size()-offset)
	n, err := file.ReadAt(tail, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	tail = tail[:n]
	if offset > 0 {
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		} else {
			tail = nil
		}
	}
	return tail, nil
}

// claudeDialogProof caches a positive transcript proof per marker version
// (its bytes and mtime): the proof records may later scroll out of the
// tail, and the wake rechecks often.
type claudeDialogProof struct {
	raw     string
	modTime time.Time
}

// answered reports whether a Claude Code marker's dialog is proven gone
// (claudeDialogAnswered). Other harnesses' markers, Startup markers, and
// markers without a transcript never are.
func (proof *claudeDialogProof) answered(raw []byte, modTime time.Time) bool {
	if proof.raw == string(raw) && proof.modTime.Equal(modTime) {
		return true
	}
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Harness != "" ||
		marker.Event == wakeDialogStartupEvent || marker.TranscriptPath == "" {
		return false
	}
	if ok, err := claudeDialogAnswered(marker, modTime); err != nil || !ok {
		return false
	}
	proof.raw, proof.modTime = string(raw), modTime
	return true
}
