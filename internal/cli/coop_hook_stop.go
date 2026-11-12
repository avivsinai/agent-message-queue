package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	// coopStopBlockLimit leaves three blocks below Claude Code's limit of
	// eight stop continuations in a row.
	coopStopBlockLimit = 5
	// coopStopStateFile holds the ids already reported in this stop chain.
	// It is advisory: a lost state reports those ids once more.
	coopStopStateFile = ".stop-hook-state.json"
)

type coopStopState struct {
	Schema      int      `json:"schema"`
	BlockedIDs  []string `json:"blocked_ids"`
	ChainBlocks int      `json:"chain_blocks"`
}

// coopStopMessage is the part of an unread message the co-op hooks read.
// Priority and File time the waiter's hold; File is nil when the inbox
// file's metadata cannot be read.
type coopStopMessage struct {
	ID       string
	Created  string
	Priority string
	File     os.FileInfo
}

// writeCoopStop is the co-op Stop guard. It blocks a stop once for unread
// mail it has not reported, so an agent does not end its turn on mail no
// doorbell will announce. It writes its state only when the state changes.
func writeCoopStop(stdin io.Reader, w io.Writer) error {
	var in struct {
		StopHookActive bool `json:"stop_hook_active"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return nil
	}
	root, me, session, err := coopHookContext()
	switch {
	case me == "":
		return nil
	case err != nil:
		return writeCoopHookJSON(w, map[string]string{"systemMessage": fmt.Sprintf("AMQ context conflict (%v); pending messages may exist. Resolve the session context and drain before stopping.", err)})
	case root == "":
		if coopStopProjectUsesAMQ() {
			return writeCoopHookJSON(w, map[string]string{"systemMessage": "AMQ context unresolved: AM_ROOT is not an absolute path; pending messages may exist. Resolve the project/session context and drain before stopping."})
		}
		return nil
	}
	pending, err := coopStopPending(root, me)
	if err != nil {
		return writeCoopHookJSON(w, map[string]string{"systemMessage": fmt.Sprintf("AMQ mailbox unreadable at %s; pending messages may exist.", root)})
	}
	var baseline *time.Time
	if len(pending) > 0 {
		if at, ok := coopHookWakeBaseline(root, me); ok {
			baseline = &at
		}
	}
	if session == "" {
		session = "(none)"
	}
	statePath := filepath.Join(fsq.AgentBase(root, me), coopStopStateFile)
	old := readCoopStopState(statePath)
	next, out := decideCoopStop(pending, baseline, old, in.StopHookActive, "root="+root+" session="+session)
	if !slices.Equal(next.BlockedIDs, old.BlockedIDs) || next.ChainBlocks != old.ChainBlocks {
		_ = writeCoopHookState(statePath, next)
	}
	if out == nil {
		return nil
	}
	return writeCoopHookJSON(w, out)
}

// decideCoopStop returns the next state and the hook output (nil to allow
// silently). Under a live wake, baseline is its startup baseline: the wake
// rings for mail created later, and mail created at or before it may be the
// cohort it baselined and never rings for, so only that mail counts. A
// message whose creation time does not parse counts.
func decideCoopStop(pending []coopStopMessage, baseline *time.Time, old coopStopState, active bool, where string) (coopStopState, map[string]string) {
	var ids []string
	for _, msg := range pending {
		if msg.ID == "" {
			continue
		}
		if baseline != nil {
			created, err := time.Parse(time.RFC3339Nano, msg.Created)
			if err == nil && created.Unix() > baseline.Unix() {
				continue
			}
		}
		ids = append(ids, msg.ID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	next := coopStopState{Schema: 1, BlockedIDs: []string{}}
	reported := coopIDSet(old.BlockedIDs)
	var fresh []string
	for _, id := range ids {
		if reported[id] {
			next.BlockedIDs = append(next.BlockedIDs, id)
		} else {
			fresh = append(fresh, id)
		}
	}
	if active && len(ids) > 0 {
		next.ChainBlocks = old.ChainBlocks
	}
	switch {
	case len(fresh) == 0:
		return next, nil
	case next.ChainBlocks >= coopStopBlockLimit:
		next.ChainBlocks = 0
		return next, map[string]string{"systemMessage": fmt.Sprintf(
			"AMQ stop-hook block budget exhausted; allowing stop and resetting the guard with %d fresh message(s) still at %s.",
			len(fresh), where)}
	default:
		next.BlockedIDs = append(next.BlockedIDs, fresh...)
		slices.Sort(next.BlockedIDs)
		next.ChainBlocks++
		return next, map[string]string{"decision": "block", "reason": fmt.Sprintf(
			"You have %d pending AMQ message(s), including %d not previously reported. Drain before stopping. %s.",
			len(ids), len(fresh), where)}
	}
}

// coopIDSet is ids as a set, for a caller that tests many ids against it.
func coopIDSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// coopStopPending reads the id and creation time of each message in
// inbox/new, skipping corrupt ones as amq list --new does.
func coopStopPending(root, me string) ([]coopStopMessage, error) {
	return new(coopInbox).pending(root, me)
}

// coopInbox reads inbox/new for a caller that looks again and again. A
// message file there never changes: it is written elsewhere and renamed in.
// So a file that was read is not read again, and a file that left the
// directory is forgotten. A file that could not be read is tried at every
// look, because the failure may pass. A look then costs one directory
// listing and no file read while nothing new arrives.
type coopInbox struct {
	seen map[string]coopStopMessage
}

func (in *coopInbox) pending(root, me string) ([]coopStopMessage, error) {
	dir := fsq.AgentInboxNew(root, me)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]coopStopMessage, len(entries))
	var pending []coopStopMessage
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		msg, known := in.seen[name]
		if !known {
			header, err := format.ReadHeaderFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			msg = coopStopMessage{ID: header.ID, Created: header.Created, Priority: header.Priority}
			if info, err := entry.Info(); err == nil {
				msg.File = info
			}
		}
		seen[name] = msg
		pending = append(pending, msg)
	}
	in.seen = seen
	return pending, nil
}

func readCoopStopState(path string) coopStopState {
	state := coopStopState{Schema: 1, BlockedIDs: []string{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	var read coopStopState
	if json.Unmarshal(raw, &read) != nil {
		return state
	}
	if read.BlockedIDs == nil {
		read.BlockedIDs = []string{}
	}
	read.Schema = 1
	return read
}

// writeCoopHookState replaces a co-op hook state file by a temp file and a
// rename, without fsync: the state is advisory.
func writeCoopHookState(path string, state any) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// coopStopProjectUsesAMQ reports whether the Claude Code project is set up
// for AMQ, so a missing AM_ROOT there may hide pending mail.
func coopStopProjectUsesAMQ() bool {
	dir := os.Getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".amqrc")); err == nil {
		return true
	}
	info, err := os.Stat(filepath.Join(dir, defaultCoopRoot))
	return err == nil && info.IsDir()
}

func writeCoopHookJSON(w io.Writer, v any) error {
	out, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", out)
	return err
}
