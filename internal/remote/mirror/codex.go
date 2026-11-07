package mirror

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

// codexSource reads a Codex thread's rollout file, which attach records as
// the binding's Transcript.
var codexSource = Source{
	Locate: codexLocate,
	Time: func(line string) int64 {
		if l, ok := codex.ParseRolloutLine(line); ok {
			return l.TS
		}
		return 0
	},
	Fold: func(st State, line string, carried Carried) (State, []Event) {
		l, ok := codex.ParseRolloutLine(line)
		if !ok {
			return st, nil
		}
		return FoldCodex(st, l, carried)
	},
}

// codexLocate is the current rollout file of the binding's thread, which
// a thread/revert moves to a new file in the sessions directory that holds
// the thread's file; else that file. The thread's file is the binding's
// Transcript, so a rebind takes effect, or, for a binding with none, the one
// rollout its proven holder process keeps open; else the record the mirror
// already reads (saved) while it is the bound thread's. "" when none is
// known, or when the current file exists only compressed. It reads only the
// date folders from the newest file of the thread it knows, so a tick's
// work does not grow with the host's Codex history.
func codexLocate(b binding.Binding, saved string) string {
	transcript := b.Transcript
	if transcript == "" {
		transcript = codex.HolderRollout(b.Holder)
	}
	sessions := codex.SessionsDir(transcript)
	thread := strings.ToLower(cmp.Or(b.AppServer.Thread, codex.RolloutThread(transcript)))
	known := transcript
	if codex.RolloutThread(saved) == thread && codex.SessionsDir(saved) == sessions {
		known = saved
	}
	if path, ok := codex.CurrentRolloutSince(sessions, thread, known, time.Now()); ok {
		return path
	}
	if transcript != "" {
		return transcript
	}
	// The saved file is read only while it is the bound thread's: a binding
	// moved to a thread with no rollout gets the note, never the old
	// thread's turns.
	if thread != "" && codex.RolloutThread(saved) == thread {
		return saved
	}
	return ""
}

// CodexCarriedID is the carrier record of a Codex clientUserMessageId: its
// SHA-256 in hex, since the id itself can be longer than a record name.
func CodexCarriedID(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return hex.EncodeToString(sum[:])
}

// Causes a Codex turn shows before its first user message names one.
var codexTriggerCauses = map[string]string{
	"daemon_recovery": "continued after a restart",
	"goal":            "goal",
}

// FoldCodex applies one rollout line. A turn is identified by the turn_id
// Codex puts on each of its events, which is both the Turn's ID and its
// PromptID. task_started opens a turn, item_completed adds to the turn it
// names, task_complete and turn_aborted end it; every other line is
// skipped.
func FoldCodex(st State, l codex.RolloutLine, carried Carried) (State, []Event) {
	if l.Type != "event_msg" || l.TurnID == "" && l.Event != "turn_aborted" {
		return st, nil
	}
	switch l.Event {
	case "task_started":
		st = st.clone()
		return st, st.codexStart(l)
	case "item_completed":
		st = st.clone()
		return st, st.codexItem(l, carried)
	case "task_complete":
		o := st.Open
		if o == nil || o.ID != l.TurnID {
			return st, nil
		}
		st = st.clone()
		o = st.Open
		if text := strings.TrimSpace(l.LastAgentMessage); text != "" {
			o.Text, o.Final = bound(text), true
		}
		o.touch(l.TS)
		kind := EndDone
		if l.Error {
			kind = EndError
			// A turn that failed before any work still shows that it failed.
			if len(o.MessageIDs) == 0 {
				o.MessageIDs = append(o.MessageIDs, o.ID)
			}
		}
		return st, st.end(kind, nil)
	case "turn_aborted":
		o := st.Open
		if o == nil || l.TurnID != "" && o.ID != l.TurnID {
			return st, nil
		}
		st = st.clone()
		st.Open.touch(l.TS)
		return st, st.end(EndInterrupted, nil)
	}
	return st, nil
}

// codexStart opens turn l.TurnID. Another open turn ends as unfinished; the
// last ended turn under the same id (a recovered turn) is reopened.
func (st *State) codexStart(l codex.RolloutLine) []Event {
	var out []Event
	if o := st.Open; o != nil {
		if o.ID == l.TurnID {
			o.touch(l.TS)
			return nil
		}
		out = st.end(EndUnfinished, out)
	}
	if last := st.Last; last != nil && last.ID == l.TurnID {
		st.Open, st.Last = last, nil
		last.Reopened, last.End = last.End, ""
		last.touch(l.TS)
		return out
	}
	cause, ok := codexTriggerCauses[l.Trigger]
	if !ok {
		cause = "prompt"
	}
	t := &Turn{ID: l.TurnID, PromptID: l.TurnID, Cause: cause, StartTS: l.TS, LastTS: l.TS}
	if st.Last != nil {
		t.StartTS = max(t.StartTS, st.Last.LastTS)
	}
	st.Open = t
	return out
}

// codexItem adds a completed item to the turn it names: the open turn, or
// the last ended one, which then reports a Continued event when the item
// changes its post (an answer or a counted tool). An item of any other turn
// is skipped.
func (st *State) codexItem(l codex.RolloutLine, carried Carried) []Event {
	it := l.Item
	tool := codexToolName(it)
	answer := it.Type == "AgentMessage" || tool != ""
	t := st.Open
	switch {
	case t != nil && t.ID == l.TurnID:
	case st.Last != nil && st.Last.ID == l.TurnID && answer:
		t = st.Last
	default:
		return nil
	}
	t.touch(l.TS)
	switch {
	case it.Type == "UserMessage":
		first := !t.Prompted
		t.Prompted = true
		switch {
		case it.ClientID != "" && carried.has(CodexCarriedID(it.ClientID)):
			if first {
				t.Cause = CauseBuzz
			}
		case !first:
		case promptCause(it.Text) == harness.Doorbell:
			t.Cause = CauseMail
		default:
			if cause := promptCause(it.Text); cause != "" {
				t.Cause = cause
			}
		}
	case it.Type == "AgentMessage":
		if it.Text != "" && (it.Phase == "final_answer" || !t.Final) {
			t.Text, t.Final = bound(it.Text), it.Phase == "final_answer"
		}
	case tool != "":
		t.count(tool)
	}
	if !answer {
		return nil
	}
	if t.FirstAssistantTS == 0 {
		t.FirstAssistantTS = l.TS
	}
	if len(t.MessageIDs) == 0 {
		t.MessageIDs = append(t.MessageIDs, cmp.Or(it.ID, t.ID))
	}
	if t == st.Last {
		return []Event{{Kind: Continued, Turn: t.clone()}}
	}
	t.Resumed = t.Reopened != ""
	return nil
}

// codexToolName is the name a post counts a tool item under; "" for an
// item that is not a tool call, or a command the user typed into the
// terminal.
func codexToolName(it codex.RolloutItem) string {
	switch it.Type {
	case "CommandExecution":
		if it.Source == "user_shell" {
			return ""
		}
		return "command"
	case "FileChange":
		return "edit"
	case "McpToolCall":
		if it.Server == "" {
			return it.Tool
		}
		return it.Server + "." + it.Tool
	case "DynamicToolCall", "CollabAgentToolCall":
		return it.Tool
	case "WebSearch":
		return "web search"
	case "ImageView":
		return "view image"
	case "Extension":
		return it.Kind
	}
	return ""
}
