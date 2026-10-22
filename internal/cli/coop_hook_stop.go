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

// coopStopMessage is the part of an unread message the guard reads.
type coopStopMessage struct {
	ID      string
	Created string
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
	me, err := normalizeHandle(os.Getenv(envMe))
	if err != nil {
		return nil
	}
	root := resolveRoot(os.Getenv(envRoot))
	if root == "" {
		if coopStopProjectUsesAMQ() {
			return writeCoopHookJSON(w, map[string]string{"systemMessage": "AMQ context unresolved: AM_ROOT is not set; pending messages may exist. Resolve the project/session context and drain before stopping."})
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
	session := inferredSessionIdentity(root)
	if session == "" {
		session = "(none)"
	}
	statePath := filepath.Join(fsq.AgentBase(root, me), coopStopStateFile)
	old := readCoopStopState(statePath)
	next, out := decideCoopStop(pending, baseline, old, in.StopHookActive, "root="+root+" session="+session)
	if !slices.Equal(next.BlockedIDs, old.BlockedIDs) || next.ChainBlocks != old.ChainBlocks {
		_ = writeCoopStopState(statePath, next)
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
	var fresh []string
	for _, id := range ids {
		if slices.Contains(old.BlockedIDs, id) {
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

// coopStopPending reads the id and creation time of each message in
// inbox/new, skipping corrupt ones as amq list --new does.
func coopStopPending(root, me string) ([]coopStopMessage, error) {
	dir := fsq.AgentInboxNew(root, me)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var pending []coopStopMessage
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		header, err := format.ReadHeaderFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		pending = append(pending, coopStopMessage{ID: header.ID, Created: header.Created})
	}
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

// writeCoopStopState replaces the state by a temp file and a rename, without
// fsync: the state is advisory.
func writeCoopStopState(path string, state coopStopState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), coopStopStateFile+".tmp.*")
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
