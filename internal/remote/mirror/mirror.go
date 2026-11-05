// Package mirror is the view of a session's turns in its Buzz channel.
// Fold assigns transcript lines to turns by the ids their writer put on them
// (message.id on an assistant line, promptId on a user line), never by line
// order alone. Render makes the one post that shows a turn, running or
// ended.
// Both are pure: no clock, file or network. The machine agent's tick reads
// the transcript, claims each post and sends it.
package mirror

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// End kinds of a turn.
const (
	EndDone           = "done"
	EndInterrupted    = "interrupted"
	EndUnfinished     = "unfinished"
	EndError          = "error"
	EndRefused        = "refused"
	EndSessionEnded   = "session ended"
	EndSessionCleared = "session cleared"
	// EndMirrorOff ends the post of a running turn when the owner turns the
	// mirror off.
	EndMirrorOff = "mirror off"
)

// Event kinds. Ended is a turn's first end; Continued changes a turn that
// already ended. Opened is a Claude turn's first assistant line that does
// not end it, Progress each later one: the turn still runs.
const (
	Opened    = "opened"
	Progress  = "progress"
	Ended     = "ended"
	Continued = "continued"
)

// Event is one turn change Fold reports, with the turn's value at that line.
// Tries counts the sends of the event whose outcome was unknown; Fold never
// sets it.
type Event struct {
	Kind  string `json:"kind"`
	Turn  Turn   `json:"turn"`
	Tries int    `json:"tries,omitempty"`
}

// ToolCount is how often a turn used one tool.
type ToolCount struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// Turn is one turn of the session: what started it, the assistant messages
// it owns, and how it ended. Times are unix milliseconds.
type Turn struct {
	// ID is the opener's uuid; PromptID the opener's promptId.
	ID       string `json:"id"`
	PromptID string `json:"prompt_id,omitempty"`
	// Cause is what started the turn, as the post shows it.
	Cause      string      `json:"cause"`
	MessageIDs []string    `json:"message_ids,omitempty"`
	Tools      []ToolCount `json:"tools,omitempty"`
	// Text is the last assistant text, trimmed, bounded by bound.
	Text string `json:"text,omitempty"`
	// StartTS is when the turn started: its opener's time, but not before
	// the previous turn's last line (an opener typed while that turn ran is
	// written with the time it was typed). FirstAssistantTS is the time of
	// the turn's first assistant line, LastTS that of its last line.
	StartTS          int64  `json:"start_ts,omitempty"`
	FirstAssistantTS int64  `json:"first_assistant_ts,omitempty"`
	LastTS           int64  `json:"last_ts,omitempty"`
	End              string `json:"end,omitempty"`
	// Answered is set when the carrier already posted the turn's answer.
	Answered bool `json:"answered,omitempty"`
	// Earlier counts the turns of a backlog that were dropped before it.
	Earlier int `json:"earlier,omitempty"`

	// Interrupted is set once the turn saw the interrupt text.
	Interrupted bool `json:"interrupted,omitempty"`
	// ClosingEnd is the end kind of the turn's closing message, "" until
	// one arrived; Closing is that message's id. The turn ends at the
	// message's text, or at the first line that is not part of it.
	Closing    string `json:"closing,omitempty"`
	ClosingEnd string `json:"closing_end,omitempty"`
	// Reopened is the end kind of an ended turn that a Stop hook continued,
	// "" otherwise; Resumed is set once it has an assistant line again. Its
	// next end is a Continued event, or, with nothing new, its old end.
	Reopened string `json:"reopened,omitempty"`
	Resumed  bool   `json:"resumed,omitempty"`

	// Prompted is set once a Codex turn read its first user message, Final
	// once its Text is a final answer, which commentary never replaces.
	Prompted bool `json:"prompted,omitempty"`
	Final    bool `json:"final,omitempty"`
}

func (t Turn) clone() Turn {
	t.MessageIDs = append([]string(nil), t.MessageIDs...)
	t.Tools = append([]ToolCount(nil), t.Tools...)
	return t
}

func (t *Turn) owns(id string) bool {
	return t != nil && id != "" && contains(t.MessageIDs, id)
}

func (t *Turn) touch(ts int64) {
	t.LastTS = max(t.LastTS, ts)
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// State is the fold between lines: the open turn and the last ended one.
type State struct {
	Open *Turn `json:"open,omitempty"`
	Last *Turn `json:"last,omitempty"`
}

func (st State) clone() State {
	if st.Open != nil {
		t := st.Open.clone()
		st.Open = &t
	}
	if st.Last != nil {
		t := st.Last.clone()
		st.Last = &t
	}
	return st
}

// Carried reports whether a frame msg_id is one the binding's carrier sent.
type Carried func(msgID string) bool

func (c Carried) has(msgID string) bool { return c != nil && msgID != "" && c(msgID) }

// Fold applies one transcript line. The end check runs before the line is
// classified, so a line that ends a turn waiting for its closing text is
// then classified with no turn open. A sidechain line and every type other
// than user and assistant (an absorbed frame parses as user) is skipped.
func Fold(st State, l claude.TranscriptLine, carried Carried) (State, []Event) {
	if l.Sidechain || l.Type != "user" && l.Type != "assistant" {
		return st, nil
	}
	st = st.clone()
	var out []Event
	if o := st.Open; o != nil && o.ClosingEnd != "" && (l.Type == "user" || l.MessageID != o.Closing) {
		out = st.end(o.ClosingEnd, out)
	}
	if l.Type == "assistant" {
		return st, st.assistant(l, out)
	}
	return st, st.user(l, carried, out)
}

// Close ends the open turn with kind (the session ended or was cleared).
func Close(st State, kind string) (State, []Event) {
	if st.Open == nil {
		return st, nil
	}
	st = st.clone()
	return st, st.end(kind, nil)
}

// end moves the open turn to Last. A turn with no assistant line emits
// nothing.
func (st *State) end(kind string, out []Event) []Event {
	t := st.Open
	st.Open, st.Last = nil, t
	t.End, t.Closing, t.ClosingEnd = kind, "", ""
	ev := Ended
	if t.Reopened != "" {
		prev, resumed := t.Reopened, t.Resumed
		t.Reopened, t.Resumed = "", false
		if !resumed {
			t.End = prev
			return out
		}
		ev = Continued
	}
	if len(t.MessageIDs) == 0 {
		return out
	}
	return append(out, Event{Kind: ev, Turn: t.clone()})
}

// assistant adds the line to the turn that owns its message.id, else to
// the open turn, which then owns the id. A line on the last ended turn
// changes that turn. A line on the open turn that does not end it reports
// Opened when it is the turn's first assistant line, else Progress.
func (st *State) assistant(l claude.TranscriptLine, out []Event) []Event {
	t := st.Open
	switch {
	case st.Open.owns(l.MessageID):
	case st.Last.owns(l.MessageID):
		t = st.Last
	case t != nil && l.MessageID != "":
		t.MessageIDs = append(t.MessageIDs, l.MessageID)
	}
	if t == nil {
		return out
	}
	text := ""
	for _, b := range l.Blocks {
		switch b.Type {
		case "tool_use":
			t.count(b.Name)
		case "text":
			text = strings.TrimSpace(strings.Join([]string{text, b.Text}, "\n"))
		}
	}
	if text != "" {
		t.Text = bound(text)
	}
	first := t.FirstAssistantTS == 0
	if first {
		t.FirstAssistantTS = l.TS
	}
	t.touch(l.TS)
	if t == st.Last {
		return append(out, Event{Kind: Continued, Turn: t.clone()})
	}
	t.Resumed = t.Reopened != ""
	if l.MessageID == "" && len(t.MessageIDs) == 0 {
		// An assistant line with no id still makes the turn one with an
		// answer.
		t.MessageIDs = append(t.MessageIDs, l.UUID)
	}
	if t.ClosingEnd == "" {
		if kind := endKind(l); kind != "" {
			t.Closing, t.ClosingEnd = l.MessageID, kind
		}
	}
	if t.ClosingEnd != "" && l.MessageID == t.Closing && text != "" {
		return st.end(t.ClosingEnd, out)
	}
	ev := Progress
	if first {
		ev = Opened
	}
	return append(out, Event{Kind: ev, Turn: t.clone()})
}

func (t *Turn) count(name string) {
	if name == "" {
		return
	}
	for i := range t.Tools {
		if t.Tools[i].Name == name {
			t.Tools[i].N++
			return
		}
	}
	t.Tools = append(t.Tools, ToolCount{Name: name, N: 1})
}

// endKind is the end a closing message gives, "" for a line that is not
// one: an API error line ends as error, refusal as refused, end_turn and
// stop_sequence as done.
func endKind(l claude.TranscriptLine) string {
	switch {
	case l.StopReason != "end_turn" && l.StopReason != "stop_sequence" && l.StopReason != "refusal":
		return ""
	case l.APIError:
		return EndError
	case l.StopReason == "refusal":
		return EndRefused
	}
	return EndDone
}

// localPrefixes open the user lines of a terminal shell command or local
// command; they never reach Buzz.
var localPrefixes = []string{"<bash-input>", "<bash-stdout>", "<bash-stderr>", "<local-command-"}

func (st *State) user(l claude.TranscriptLine, carried Carried, out []Event) []Event {
	text, hasText, result := "", false, false
	for _, b := range l.Blocks {
		switch b.Type {
		case "text":
			if !hasText {
				text, hasText = b.Text, true
			}
		case "tool_result":
			result = true
		}
	}
	o := st.Open
	switch {
	case l.Absorbed:
		if o != nil {
			o.Answered = o.Answered || carried.has(l.MsgID)
			o.touch(l.TS)
		}
		return out
	case o != nil && l.PromptID != "" && l.PromptID == o.PromptID:
		o.Interrupted = o.Interrupted || strings.HasPrefix(text, claude.InterruptPrefix)
		o.touch(l.TS)
		return out
	case o == nil && strings.HasPrefix(text, "<bash-input>"):
		// A shell command typed with no turn open starts one, which posts
		// only if the model answers it. The command itself is never shown.
		st.open(l, "shell command")
		return out
	case hasPrefix(text, localPrefixes...):
		return out
	case o == nil && l.Meta && st.Last != nil && l.PromptID != "" && l.PromptID == st.Last.PromptID:
		st.Open, st.Last = st.Last, nil
		st.Open.Reopened, st.Open.End = st.Open.End, ""
		st.Open.touch(l.TS)
		return out
	case strings.HasPrefix(text, claude.InterruptPrefix):
		// The interrupt text can carry the next prompt's promptId: it marks
		// the running turn and never starts one.
		if o != nil {
			o.Interrupted = true
			o.touch(l.TS)
		}
		return out
	case !hasText || result || l.CompactSummary || l.VisibleOnly || l.Meta && !metaOpener(l):
		return out
	}
	if o != nil {
		kind := EndUnfinished
		if o.Interrupted {
			kind = EndInterrupted
		}
		out = st.end(kind, out)
	}
	t := st.open(l, "")
	switch {
	case l.OriginKind == "peer" && carried.has(l.MsgID):
		t.Cause, t.Answered = CauseBuzz, true
	case promptCause(text) == harness.Doorbell:
		// Typed into the terminal, or sent as a peer frame by a native wake.
		t.Cause = CauseMail
	case l.OriginKind == "peer", l.OriginKind == "" && l.CrossSession:
		t.Cause = "message from another session"
	case l.OriginKind == "task-notification":
		t.Cause = "background task finished"
	case l.OriginKind == "auto-continuation":
		t.Cause = "continued"
	case l.TurnOrigin == "scheduled":
		t.Cause = "scheduled task"
	default:
		t.Cause = promptCause(text)
	}
	return out
}

// metaOpener reports whether a harness-injected (meta) line starts a turn:
// an auto-continuation, a background task's notification, or a scheduled
// fire.
func metaOpener(l claude.TranscriptLine) bool {
	return l.OriginKind == "auto-continuation" || l.OriginKind == "task-notification" || l.TurnOrigin == "scheduled"
}

// open makes l the opener of the open turn. The turn starts no earlier than
// the previous turn's last line.
func (st *State) open(l claude.TranscriptLine, cause string) *Turn {
	t := &Turn{ID: l.UUID, PromptID: l.PromptID, Cause: cause, StartTS: l.TS, LastTS: l.TS}
	if st.Last != nil {
		t.StartTS = max(t.StartTS, st.Last.LastTS)
	}
	st.Open = t
	return t
}

func hasPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// CauseBuzz names a turn that answered a Buzz message. CauseMail names a
// turn the AMQ doorbell started: the doorbell text cannot tell whose mail
// it was, so the machine agent turns CauseMail into CauseBuzz once it sees
// the turn's reply in the buzz inbox.
const (
	CauseBuzz = "Buzz"
	CauseMail = "AMQ message"
)

// causeMax bounds the prompt text a post shows, in characters.
const causeMax = 200

var (
	commandName = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	leadingTag  = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9_-]*)[^>]*>`)
)

// markupCauses name a prompt that starts with harness markup.
var markupCauses = []struct{ prefix, cause string }{
	{"[Image", "image"},
	{"<pasted_content", "pasted text"},
	{"<teammate-message", "message from a teammate"},
}

// promptCause is a typed prompt as the post shows it: a slash command's
// name; a name for an image, pasted text or a teammate's message; else the
// first line of its text outside leading tags, at most causeMax characters,
// or "prompt" when no text is outside them.
func promptCause(text string) string {
	if m := commandName.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	text = strings.TrimSpace(text)
	for _, m := range markupCauses {
		if strings.HasPrefix(text, m.prefix) {
			return m.cause
		}
	}
	tagged := false
	for {
		m := leadingTag.FindStringSubmatch(text)
		if m == nil {
			break
		}
		tagged = true
		rest := text[len(m[0]):]
		if _, after, ok := strings.Cut(rest, "</"+m[1]+">"); ok {
			rest = after
		}
		text = strings.TrimSpace(rest)
	}
	first, _, _ := strings.Cut(text, "\n")
	first = strings.TrimSpace(first)
	if first == "" && tagged {
		return "prompt"
	}
	if r := []rune(first); len(r) > causeMax {
		first = string(r[:causeMax]) + "…"
	}
	return first
}

// TextMax bounds the answer text of a post, in bytes.
const TextMax = 3500

// cutNote ends an answer that was cut.
const cutNote = "… (cut; the full answer is in the session)"

// Render is the one post that shows t:
//
//	▶ <cause>
//	<mark> <N tools (names)> · <duration>
//	<final assistant text>
//
// A turn that still runs (no End) or that the carrier already answered
// shows the first two lines only.
func Render(t Turn) string {
	var b strings.Builder
	b.WriteString("▶ " + t.Cause + "\n")
	parts := []string{endMark(t.End)}
	if n := toolTotal(t.Tools); n > 0 {
		names := make([]string, 0, len(t.Tools))
		for _, c := range t.Tools {
			if c.N > 1 {
				names = append(names, fmt.Sprintf("%s ×%d", c.Name, c.N))
			} else {
				names = append(names, c.Name)
			}
		}
		unit := "tools"
		if n == 1 {
			unit = "tool"
		}
		parts = append(parts, fmt.Sprintf("%d %s (%s)", n, unit, strings.Join(names, ", ")))
	}
	if t.StartTS > 0 && t.LastTS >= t.StartTS {
		parts = append(parts, (time.Duration(t.LastTS-t.StartTS) * time.Millisecond).Round(time.Second).String())
	}
	b.WriteString(parts[0])
	for i, p := range parts[1:] {
		if i == 0 && t.End == EndDone {
			b.WriteString(" " + p)
			continue
		}
		b.WriteString(" · " + p)
	}
	if text := t.Text; text != "" && t.End != "" && !t.Answered {
		b.WriteString("\n" + cut(text))
	}
	if t.Earlier > 0 {
		fmt.Fprintf(&b, "\n(+%d earlier turns not shown)", t.Earlier)
	}
	return unmention(b.String())
}

// wordJoiner is invisible and is not a name character.
const wordJoiner = "\u2060"

// unmention puts a word joiner after each "@" that starts a word, so a post
// never mentions anyone. Buzz reads "@name" after a space or at the start
// as a mention: it refuses a post whose name matches no channel member, and
// pings a member whose name matches.
func unmention(s string) string {
	var b strings.Builder
	for i, r := range s {
		b.WriteRune(r)
		if r == '@' && (i == 0 || strings.IndexByte(" \t\n\v\f\r", s[i-1]) >= 0) {
			b.WriteString(wordJoiner)
		}
	}
	return b.String()
}

func endMark(kind string) string {
	switch kind {
	case "":
		return "… running"
	case EndDone:
		return "✓"
	case EndSessionEnded, EndSessionCleared, EndMirrorOff:
		return "■ ended (" + kind + ")"
	}
	return "✗ " + kind
}

func toolTotal(tools []ToolCount) int {
	n := 0
	for _, c := range tools {
		n += c.N
	}
	return n
}

// bound keeps the first TextMax+1 bytes of text, extended to the next rune
// start: enough for cut to give the same post, whatever the text's length.
func bound(text string) string {
	if len(text) <= TextMax+1 {
		return text
	}
	end := TextMax + 1
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	return text[:end]
}

// cut bounds text at TextMax bytes, at a line end where one falls inside
// the bound.
func cut(text string) string {
	if len(text) <= TextMax {
		return text
	}
	end := TextMax
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	if i := strings.LastIndexByte(text[:end], '\n'); i > 0 {
		end = i
	}
	return strings.TrimRight(text[:end], "\n") + "\n" + cutNote
}

// Root is the mirror's state directory, beside the binding file:
// ~/.amq/remote/mirror. Each binding has a directory below it.
func Root() (string, error) {
	path, err := binding.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "mirror"), nil
}

// offFile is present in a binding's mirror directory while the owner has
// the mirror off.
const offFile = "off"

// Off reports whether the mirror of the binding whose mirror directory is
// dir is off.
func Off(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, offFile))
	return err == nil && fi.Mode().IsRegular()
}

// SetOff turns the mirror of binding name off or on. The machine agent
// reads the switch on every tick: while it is off the fold still runs and
// nothing is queued, so turning it on posts no turn from the off time.
func SetOff(name string, off bool) error {
	dir, err := DirFor(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, offFile)
	if !off {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// DirFor is binding name's mirror directory; it is not created.
func DirFor(name string) (string, error) {
	if name != binding.LegacyName {
		if err := binding.ValidName(name); err != nil {
			return "", err
		}
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// msgIDRe matches what a native carrier sends a request as: a frame msg_id,
// or a request reference.
var msgIDRe = regexp.MustCompile(`^(?:[0-9a-f-]{1,64}|` + protocol.RefPrefix + `[a-z2-7]{16,` + strconv.Itoa(protocol.MaxRefLen) + `})$`)

// RecordCarried records, synced, that the native carrier of binding name
// sends a frame with msgID. It is written before the frame is sent, so a
// transcript line with that msg_id always finds it.
func RecordCarried(name, msgID string) error {
	if !msgIDRe.MatchString(msgID) {
		return fmt.Errorf("msg_id %q is not a frame msg_id or a request reference", msgID)
	}
	dir, err := DirFor(name)
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "carrier")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_, err = fsq.CreateNewFile(filepath.Join(dir, msgID), nil, 0o600)
	return err
}

// CarriedIn is the Carried of the mirror directory dir.
func CarriedIn(dir string) Carried {
	return func(msgID string) bool {
		if !msgIDRe.MatchString(msgID) {
			return false
		}
		fi, err := os.Lstat(filepath.Join(dir, "carrier", msgID))
		return err == nil && fi.Mode().IsRegular()
	}
}
