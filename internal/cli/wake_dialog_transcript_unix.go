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
	"regexp"
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
// opened (the batch that asked) is not a new message. A subagent's dialog
// (the hook input names its agent_id) blocks only that subagent while the
// main chain runs on, so only the subagent's own transcript can prove it;
// A permission hook gives no agent_transcript_path, so the wake finds that
// transcript by the layout Claude Code writes (claudeSubagentTranscript).

// maxWakeDialogTranscriptTail bounds each transcript read; the records that
// prove a closed dialog are the newest ones.
const maxWakeDialogTranscriptTail = 256 << 10

type claudeTranscriptRecord struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Timestamp   string `json:"timestamp"`
	Message     struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeTranscriptBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
}

// claudeDialogAnswered reports whether the transcript the marker names
// proves its dialog gone: a tool_result for the marker's tool_use_id (in a
// subagent's own transcript when the marker names one), or for
// a permission marker (claudePermissionMarker) a new assistant message of
// its own chain after it opened (claudeTurnMovedOn): the main chain, or for
// a subagent's marker only that subagent's transcript. Any other marker is
// never proven answered. An error means no proof: the hold stays.
func claudeDialogAnswered(marker wakeDialogMarker, modTime time.Time) (bool, error) {
	return claudeDialogAnsweredFrom(marker, modTime, readClaudeTranscriptTail)
}

// claudeTranscriptReader reads a transcript tail (readClaudeTranscriptTail).
type claudeTranscriptReader func(path string) ([]byte, error)

// newClaudeTranscriptReader reads each transcript tail once and returns the
// same result to every later call.
func newClaudeTranscriptReader() claudeTranscriptReader {
	type result struct {
		tail []byte
		err  error
	}
	read := make(map[string]result)
	return func(path string) ([]byte, error) {
		if r, ok := read[path]; ok {
			return r.tail, r.err
		}
		tail, err := readClaudeTranscriptTail(path)
		read[path] = result{tail, err}
		return tail, err
	}
}

// claudeDialogAnsweredFrom is claudeDialogAnswered reading through read.
func claudeDialogAnsweredFrom(marker wakeDialogMarker, modTime time.Time, read claudeTranscriptReader) (bool, error) {
	permission := claudePermissionMarker(marker)
	if marker.ToolUseID == "" && !permission {
		return false, nil
	}
	if permission && marker.AgentID != "" {
		// A subagent's dialog: only its own transcript, every record of
		// which is the subagent's, proves it closed.
		path, err := claudeSubagentTranscript(marker)
		if err != nil {
			return false, err
		}
		tail, err := read(path)
		if err != nil {
			return false, err
		}
		return claudeTurnMovedOn(tail, wakeDialogMarkerOpened(marker, modTime), true), nil
	}
	path := marker.TranscriptPath
	if marker.AgentID != "" && marker.AgentTranscriptPath != "" {
		path = marker.AgentTranscriptPath
	}
	tail, err := read(path)
	if err != nil {
		return false, err
	}
	if permission {
		return claudeTurnMovedOn(tail, wakeDialogMarkerOpened(marker, modTime), false), nil
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

// claudeTranscriptNameRe is a session or subagent id that names exactly one
// path component.
var claudeTranscriptNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// claudeSubagentTranscript is the transcript of the marker's subagent: its
// agent_transcript_path when the hook gave one, else the path Claude Code
// lays out beside the session transcript <dir>/<session_id>.jsonl:
// <dir>/<session_id>/subagents/agent-<agent_id>.jsonl, or one or two
// subdirectories deeper (a workflow's subagents write to
// subagents/workflows/<wf>/). Invariant: the proof reads only a regular
// file, reached through no symlink below <dir>, named for exactly this
// agent_id, and found exactly once across the three depths; anything else
// is an error and no proof.
func claudeSubagentTranscript(marker wakeDialogMarker) (string, error) {
	if marker.AgentTranscriptPath != "" {
		return marker.AgentTranscriptPath, nil
	}
	if !claudeTranscriptNameRe.MatchString(marker.AgentID) || !claudeTranscriptNameRe.MatchString(marker.SessionID) ||
		!filepath.IsAbs(marker.TranscriptPath) || filepath.Base(marker.TranscriptPath) != marker.SessionID+".jsonl" {
		return "", errors.New("no subagent transcript path for this marker")
	}
	session := filepath.Join(filepath.Dir(marker.TranscriptPath), marker.SessionID)
	subagents := filepath.Join(session, "subagents")
	for _, dir := range []string{session, subagents} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return "", fmt.Errorf("subagent transcripts: %s is not a directory", dir)
		}
	}
	name := "agent-" + marker.AgentID + ".jsonl"
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

// claudeTranscriptSyntheticModel marks an assistant record Claude Code
// writes itself, such as an API error, which can land while a dialog shows.
const claudeTranscriptSyntheticModel = "<synthetic>"

// claudeTurnMovedOn reports whether tail holds an assistant record of the
// chain dated after opened whose message has no record dated at or before
// opened. ownChain is a subagent's own transcript; otherwise sidechain
// records belong to other chains and are skipped. Synthetic records never
// count.
func claudeTurnMovedOn(tail []byte, opened time.Time, ownChain bool) bool {
	before := make(map[string]bool)
	var after []string
	for line := range bytes.SplitSeq(tail, []byte{'\n'}) {
		var record claudeTranscriptRecord
		if json.Unmarshal(line, &record) != nil || record.Type != "assistant" || (record.IsSidechain && !ownChain) ||
			record.Message.ID == "" || record.Message.Model == claudeTranscriptSyntheticModel {
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
func (proof *claudeDialogProof) answered(raw []byte, modTime time.Time, read claudeTranscriptReader) bool {
	if proof.raw == string(raw) && proof.modTime.Equal(modTime) {
		return true
	}
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Harness != "" ||
		marker.Event == wakeDialogStartupEvent || marker.TranscriptPath == "" {
		return false
	}
	if ok, err := claudeDialogAnsweredFrom(marker, modTime, read); err != nil || !ok {
		return false
	}
	proof.raw, proof.modTime = string(raw), modTime
	return true
}
