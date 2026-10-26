//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// Esc or a deny on a Claude Code permission dialog runs no hook, but Claude
// Code 2.1.310 writes it to the session transcript at once (observed live):
// a user record with a tool_result for the asked tool call (is_error, "User
// rejected tool use"), then a user record whose text starts with
// claudeTranscriptInterruptPrefix. The wake reads the transcript read-only
// as proof that the dialog is gone; without those records the hold stays.

const (
	// maxWakeDialogTranscriptTail bounds each transcript read; the records
	// that prove a closed dialog are the newest ones.
	maxWakeDialogTranscriptTail = 256 << 10
	// claudeTranscriptInterruptPrefix starts the text Claude Code records
	// when the person ends a turn with Esc or a deny on a dialog.
	claudeTranscriptInterruptPrefix = "[Request interrupted by user"
)

type claudeTranscriptRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeTranscriptBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Text      string          `json:"text"`
}

// claudeDialogAnswered reports whether the transcript the marker names shows
// its dialog gone: a tool_result for the marker's tool call, or an
// interrupt record dated after openedAt (the marker's mtime). The tool call
// is the marker's tool_use_id, or the newest tool_use whose name and input
// match its input digest; a call is recorded before its PermissionRequest
// runs, so an older identical call never stands in for it. An error means
// no proof: the hold stays.
func claudeDialogAnswered(marker wakeDialogMarker, openedAt time.Time) (bool, error) {
	tail, err := readClaudeTranscriptTail(marker.TranscriptPath)
	if err != nil {
		return false, err
	}
	toolUseID := marker.ToolUseID
	results := make(map[string]bool)
	interrupted := false
	for line := range bytes.SplitSeq(tail, []byte{'\n'}) {
		var record claudeTranscriptRecord
		if json.Unmarshal(line, &record) != nil ||
			(record.Type != "user" && record.Type != "assistant") {
			continue
		}
		var blocks []claudeTranscriptBlock
		if json.Unmarshal(record.Message.Content, &blocks) != nil {
			continue // a typed prompt is a string
		}
		for _, block := range blocks {
			switch {
			case record.Type == "assistant" && block.Type == "tool_use":
				if marker.ToolUseID == "" && marker.InputDigest != "" &&
					wakeDialogInputDigest(wakeDialogHookInput{Tool: block.Name, ToolInput: block.Input}) == marker.InputDigest {
					toolUseID = block.ID
				}
			case record.Type == "user" && block.Type == "tool_result":
				results[block.ToolUseID] = true
			case record.Type == "user" && block.Type == "text" &&
				strings.HasPrefix(block.Text, claudeTranscriptInterruptPrefix):
				if at, err := time.Parse(time.RFC3339Nano, record.Timestamp); err == nil && at.After(openedAt) {
					interrupted = true
				}
			}
		}
	}
	return interrupted || (toolUseID != "" && results[toolUseID]), nil
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

// answered reports whether a Claude Code marker's dialog is proven gone.
// Other harnesses' markers, Startup markers and markers without a
// transcript never are.
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
