package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
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
	NoticedIDs  []string `json:"noticed_ids,omitempty"`
	ChainBlocks int      `json:"chain_blocks"`
}

// coopStopMessage is the part of an unread message the co-op hooks read.
// Priority and File time the waiter's hold; File is nil when the inbox
// file's metadata cannot be read.
type coopStopMessage struct {
	ID       string
	Created  string
	Priority string
	Passive  bool
	File     os.FileInfo
}

// writeCoopPrompt lets a prompt carry unread mail to the model without a
// keystroke. It creates no state and never blocks or disturbs the prompt.
func writeCoopPrompt(stdin io.Reader, w io.Writer) error {
	var in struct {
		Prompt string `json:"prompt"`
	}
	// The bound can cut a long prompt. A cut prompt is not the doorbell, so a
	// decode error only means there is no prompt to compare.
	_ = json.NewDecoder(io.LimitReader(stdin, coopPromptInputLimit)).Decode(&in)
	root, me, session, err := coopHookContext()
	if me == "" || root == "" || err != nil {
		return nil
	}
	writeCoopPromptFor(w, in.Prompt, root, me, session)
	return nil
}

// coopPromptInputLimit bounds the hook input a prompt receiver reads.
const coopPromptInputLimit = 1 << 20

// writeCoopPromptFor prints the unread-mail context of (root, me) for a
// submitted prompt. The wake's own doorbell gets none: it already tells the
// agent to drain.
func writeCoopPromptFor(w io.Writer, prompt, root, me, session string) {
	if harness.IsDoorbell(prompt) {
		return
	}
	pending, err := coopStopPending(root, me)
	if err != nil {
		return
	}
	unread := 0
	for _, msg := range pending {
		if msg.ID != "" {
			unread++
		}
	}
	if unread == 0 {
		return
	}
	if session == "" {
		session = "(none)"
	}
	_ = writeCoopHookJSON(w, map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": fmt.Sprintf("You have %d unread AMQ message(s). Run amq drain --include-body. root=%s session=%s.", unread, root, session),
		},
	})
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
	statePath := filepath.Join(fsq.AgentBase(root, me), coopStopStateFile)
	old := readCoopStopState(statePath)
	// The seat decision is read before the wake baseline, and only when mail
	// is pending or a notice is already recorded: most turn ends have
	// neither, and the decision's reads are the only ones here that touch
	// the disk. The guard asks it only whether the seat is attended: every
	// other live wake, whatever its channel, rings or yields to a ringer for
	// mail after its baseline.
	attended := false
	if len(pending) > 0 || len(old.NoticedIDs) > 0 {
		attended = readSeatDoorbell(root, me).ringer == doorbellNone
	}
	var baseline *time.Time
	if len(pending) > 0 && !attended {
		if at, ok := coopHookWakeBaseline(root, me); ok {
			baseline = &at
		}
	}
	if session == "" {
		session = "(none)"
	}
	// An attended seat never blocks: the person reads the systemMessage and
	// the model learns of the mail at their next prompt.
	if attended {
		return writeCoopStopAttended(w, pending, old, statePath)
	}
	next, out := decideCoopStop(pending, baseline, old, in.StopHookActive, "root="+root+" session="+session)
	if !slices.Equal(next.BlockedIDs, old.BlockedIDs) || next.ChainBlocks != old.ChainBlocks || !slices.Equal(next.NoticedIDs, old.NoticedIDs) {
		_ = writeCoopHookState(statePath, next)
	}
	if out == nil {
		return nil
	}
	return writeCoopHookJSON(w, out)
}

// writeCoopStopAttended reports every unread message once, with no wake
// baseline: an attended wake types nothing, so mail after its baseline would
// otherwise never be reported at a turn end. An id already reported by a
// block is noticed too, so attended off-on-off does not print it a third time.
// Claude's historical advisory writes stay advisory; Codex writes may fail
// silently before a notice, without one duplicate per subsequent turn.
func writeCoopStopAttended(w io.Writer, pending []coopStopMessage, old coopStopState, statePath string) error {
	return writeCoopStopAttendedState(w, pending, old, statePath, true)
}

func writeCoopStopAttendedState(w io.Writer, pending []coopStopMessage, old coopStopState, statePath string, advisory bool) error {
	next := decideCoopStopAttended(pending, old)
	fresh := freshAttendedIDs(pending, old.NoticedIDs, old.BlockedIDs)
	if !slices.Equal(next.NoticedIDs, old.NoticedIDs) || !slices.Equal(next.BlockedIDs, old.BlockedIDs) {
		if err := writeCoopHookState(statePath, next); err != nil && !advisory {
			return err
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	return writeCoopHookJSON(w, map[string]string{"systemMessage": fmt.Sprintf(
		"AMQ: %d unread message(s). Run amq drain --include-body.", len(pending))})
}

// decideCoopStopAttended is the attended-seat Stop state: noticed = the
// ids that are pending now, in NoticedIDs. BlockedIDs and the chain block
// count stay as they are: an attended seat never blocks, and a blocked id
// counts as noticed through freshAttendedIDs.
func decideCoopStopAttended(pending []coopStopMessage, old coopStopState) coopStopState {
	ids := make([]string, 0, len(pending))
	for _, msg := range pending {
		if msg.ID != "" {
			ids = append(ids, msg.ID)
		}
	}
	slices.Sort(ids)
	return coopStopState{Schema: 1, NoticedIDs: slices.Compact(ids), BlockedIDs: old.BlockedIDs, ChainBlocks: old.ChainBlocks}
}

// freshAttendedIDs is the pending ids the attended Stop guard has not
// noticed yet: neither a systemMessage nor an earlier block reported them.
// There is no wake-baseline cut: an attended wake types nothing, so mail
// created after its baseline is still only reported here.
func freshAttendedIDs(pending []coopStopMessage, noticed, blocked []string) []string {
	seen := coopIDSet(noticed)
	for _, id := range blocked {
		seen[id] = true
	}
	var fresh []string
	for _, msg := range pending {
		if msg.ID == "" || seen[msg.ID] {
			continue
		}
		fresh = append(fresh, msg.ID)
	}
	slices.Sort(fresh)
	return fresh
}

// decideCoopStop returns the next state and the hook output (nil to allow
// silently). Under a live wake, baseline is its startup baseline: the wake
// rings for mail created later, and mail created at or before it may be the
// cohort it baselined and never rings for, so only that mail counts. A
// message whose creation time does not parse counts.
func decideCoopStop(pending []coopStopMessage, baseline *time.Time, old coopStopState, active bool, where string) (coopStopState, map[string]string) {
	var ids []string
	for _, msg := range pending {
		if msg.ID == "" || msg.Passive {
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

	next := coopStopState{Schema: 1, NoticedIDs: old.NoticedIDs, BlockedIDs: []string{}}
	// Drained mail leaves the state: keep only ids still pending, so the
	// state does not grow with every message ever reported (the same filter
	// decideCoopStopAttended applies on the attended path). The set is every
	// pending message, not the baseline-filtered ids: a noticed id the wake
	// does not ring for is still unread and must stay noticed.
	pendingSeen := make(map[string]bool, len(pending))
	for _, msg := range pending {
		if msg.ID != "" {
			pendingSeen[msg.ID] = true
		}
	}
	kept := make([]string, 0, len(old.NoticedIDs))
	for _, id := range old.NoticedIDs {
		if pendingSeen[id] {
			kept = append(kept, id)
		}
	}
	slices.Sort(kept)
	next.NoticedIDs = slices.Compact(kept)
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
			var malformed *json.UnmarshalTypeError
			if err != nil && (!errors.As(err, &malformed) || malformed.Field != "notify" || header.ID == "") {
				continue
			}
			msg = coopStopMessage{ID: header.ID, Created: header.Created, Priority: header.Priority, Passive: err == nil && !wakeEligible(header)}
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
	state, _ := readCoopStopStateChecked(path)
	return state
}

// readCoopStopStateChecked keeps absence distinct from corrupt or unreadable
// state. Claude uses the historical collapsing wrapper above unchanged.
func readCoopStopStateChecked(path string) (coopStopState, error) {
	state := coopStopState{Schema: 1, BlockedIDs: []string{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	var read coopStopState
	if err := json.Unmarshal(raw, &read); err != nil {
		return state, err
	}
	if read.BlockedIDs == nil {
		read.BlockedIDs = []string{}
	}
	read.Schema = 1
	return read, nil
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
