//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
// the wake folds every tool call of the whole transcript
// (claudeTranscriptFold) and binds the marker to a tool_use id only when
// exactly one call of the main conversation with that name and input was
// pending when the marker opened, and no other equal call is pending. A
// tail alone cannot show that: the dialog's own call can sit before it. An
// equal call that
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
	tail, err := readClaudeTranscriptTail(marker.TranscriptPath)
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

// maxWakeDialogTranscriptBytes bounds the transcript the wake folds for a
// permission marker; a larger one is never used to bind.
const maxWakeDialogTranscriptBytes = 64 << 20

// maxWakeDialogTranscriptLine bounds one transcript record, as much as a
// hook input may carry. A longer record leaves the fold incomplete.
const maxWakeDialogTranscriptLine = maxWakeDialogHookInputBytes

// claudeTranscriptCall is one main or subagent tool_use the fold has seen.
type claudeTranscriptCall struct {
	// digest is claude.ActionHash of the call's tool name and input.
	digest string
	// at and resultAt are the record stamps, zero when unstamped.
	at       time.Time
	side     bool
	resulted bool
	resultAt time.Time
}

// claudeTranscriptFold is the wake's complete inventory of the tool calls
// in one Claude Code transcript: every tool_use from byte 0, and whether
// and when its tool_result came. Each use reads only the bytes appended
// since the last one; a file that shrank or was replaced is folded again
// from byte 0. It lives in wake memory only. The remote attachment's
// per-run call list cannot stand in for it: that list starts near the end
// of the file and drops answered calls, so it never proves that no other
// equal call is pending.
type claudeTranscriptFold struct {
	mu     sync.Mutex
	path   string
	file   os.FileInfo
	offset int64
	// anchor is the last bytes folded, up to offset; other bytes there
	// mean the file was rewritten in place.
	anchor []byte
	calls  map[string]*claudeTranscriptCall
	// incomplete is the fixed reason the inventory cannot be complete; it
	// holds until the file is replaced.
	incomplete string
}

// reset starts the fold of path over from byte 0.
func (fold *claudeTranscriptFold) reset(path string, info os.FileInfo) {
	fold.path, fold.file, fold.offset, fold.anchor = path, info, 0, nil
	fold.calls = make(map[string]*claudeTranscriptCall)
	fold.incomplete = ""
}

// advance folds the complete records appended to path since the last call.
// The open follows no symlink and accepts only a regular file. A partial
// trailing record is read again next time. An error means the transcript
// cannot be read now; the fold keeps what it has.
func (fold *claudeTranscriptFold) advance(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("transcript path %q is not absolute", path)
	}
	file, info, err := fsq.OpenRegularNoFollowBounded(path, 0)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()
	size := info.Size()
	if path != fold.path || fold.file == nil || !os.SameFile(fold.file, info) || size < fold.offset ||
		!fold.anchored(file) {
		fold.reset(path, info)
	}
	fold.file = info
	switch {
	case fold.incomplete != "":
		return nil
	case size > maxWakeDialogTranscriptBytes:
		fold.incomplete = fmt.Sprintf("transcript over %d MiB", maxWakeDialogTranscriptBytes>>20)
		return nil
	}
	for fold.offset < size {
		buf := make([]byte, min(size-fold.offset, maxWakeDialogTranscriptLine+1))
		n, err := file.ReadAt(buf, fold.offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read transcript: %w", err)
		}
		buf = buf[:n]
		end := bytes.LastIndexByte(buf, '\n')
		if end < 0 {
			if len(buf) > maxWakeDialogTranscriptLine {
				fold.incomplete = fmt.Sprintf("a transcript record over %d MiB", maxWakeDialogTranscriptLine>>20)
			}
			return nil // a record still being written
		}
		for line := range bytes.SplitSeq(buf[:end], []byte{'\n'}) {
			if len(line) > maxWakeDialogTranscriptLine {
				fold.incomplete = fmt.Sprintf("a transcript record over %d MiB", maxWakeDialogTranscriptLine>>20)
				return nil
			}
			fold.add(line)
		}
		fold.offset += int64(end) + 1
		fold.anchor = append([]byte(nil), buf[max(0, end+1-wakeDialogTranscriptAnchor):end+1]...)
	}
	return nil
}

// wakeDialogTranscriptAnchor is how many folded bytes the fold rechecks.
const wakeDialogTranscriptAnchor = 64

// anchored reports whether the file still holds the folded bytes just
// before the fold's offset.
func (fold *claudeTranscriptFold) anchored(file *os.File) bool {
	if len(fold.anchor) == 0 {
		return true
	}
	got := make([]byte, len(fold.anchor))
	n, err := file.ReadAt(got, fold.offset-int64(len(fold.anchor)))
	return (err == nil || errors.Is(err, io.EOF)) && n == len(got) && bytes.Equal(got, fold.anchor)
}

// add folds one record: an assistant record's tool_use blocks and a user
// record's tool_result blocks. Other records, api-request-blob copies
// among them, are not calls.
func (fold *claudeTranscriptFold) add(line []byte) {
	if !bytes.Contains(line, []byte(`"tool_`)) {
		return
	}
	record, blocks := claudeTranscriptBlocks(line)
	at, _ := time.Parse(time.RFC3339Nano, record.Timestamp) // zero when unstamped
	for _, block := range blocks {
		switch {
		case record.Type == "assistant" && block.Type == "tool_use" && block.ID != "":
			if _, seen := fold.calls[block.ID]; seen {
				continue
			}
			digest, _ := claude.ActionHash(block.Name, block.Input)
			fold.calls[block.ID] = &claudeTranscriptCall{digest: digest, at: at, side: record.IsSidechain || record.AgentID != ""}
		case record.Type == "user" && block.Type == "tool_result":
			if call := fold.calls[block.ToolUseID]; call != nil && !call.resulted {
				call.resulted, call.resultAt = true, at
			}
		}
	}
}

// permissionCall finds the one tool call an unbound permission marker's
// dialog stands for, from the complete inventory of its transcript. id is
// set only when exactly one main conversation call with the marker's input
// digest was pending at the marker's open time (stamped at or before it,
// with no tool_result stamped at or before it) and no other equal call is
// pending at that time or now. Otherwise why says, in fixed text, why the
// marker stays unbound. err is a transcript that cannot be read.
func (fold *claudeTranscriptFold) permissionCall(marker wakeDialogMarker) (id, why string, err error) {
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
	fold.mu.Lock()
	defer fold.mu.Unlock()
	if err := fold.advance(marker.TranscriptPath); err != nil {
		return "", "", err
	}
	if fold.incomplete != "" {
		return "", fold.incomplete, nil
	}
	var pending []string
	side := false
	for callID, call := range fold.calls {
		if call.digest != marker.InputDigest {
			continue
		}
		pendingNow := !call.resulted
		// An unstamped result has no time, so it cannot show the call was
		// still pending when the marker opened.
		pendingAtOpen := !call.at.IsZero() && !call.at.After(opened) &&
			(!call.resulted || (!call.resultAt.IsZero() && call.resultAt.After(opened)))
		switch {
		case !pendingNow && !pendingAtOpen:
		case call.side:
			side = true
		default:
			pending = append(pending, callID)
		}
	}
	switch {
	case side:
		return "", "ambiguous: a subagent call matches", nil
	case len(pending) == 0:
		return "", "no pending call in the transcript", nil
	case len(pending) > 1:
		return "", fmt.Sprintf("ambiguous: %d pending calls", len(pending)), nil
	}
	only := fold.calls[pending[0]]
	if only.at.IsZero() || only.at.After(opened) {
		return "", "no call pending when the dialog opened", nil
	}
	return pending[0], "", nil
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
