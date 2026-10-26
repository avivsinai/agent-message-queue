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
// gone; without that record the hold stays. A permission marker has no
// tool_use_id, so no transcript record proves it closed.

// maxWakeDialogTranscriptTail bounds each transcript read; the records that
// prove a closed dialog are the newest ones.
const maxWakeDialogTranscriptTail = 256 << 10

type claudeTranscriptRecord struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeTranscriptBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
}

// claudeDialogAnswered reports whether the transcript the marker names
// holds a tool_result for the marker's tool_use_id. A marker without one is
// never proven answered. An error means no proof: the hold stays.
func claudeDialogAnswered(marker wakeDialogMarker) (bool, error) {
	if marker.ToolUseID == "" {
		return false, nil
	}
	tail, err := readClaudeTranscriptTail(marker.TranscriptPath)
	if err != nil {
		return false, err
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
// Other harnesses' markers, Startup markers, and markers without a
// transcript or a tool_use_id never are.
func (proof *claudeDialogProof) answered(raw []byte, modTime time.Time) bool {
	if proof.raw == string(raw) && proof.modTime.Equal(modTime) {
		return true
	}
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Harness != "" ||
		marker.Event == wakeDialogStartupEvent || marker.TranscriptPath == "" || marker.ToolUseID == "" {
		return false
	}
	if ok, err := claudeDialogAnswered(marker); err != nil || !ok {
		return false
	}
	proof.raw, proof.modTime = string(raw), modTime
	return true
}
