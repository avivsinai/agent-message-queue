package claude

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// PermissionRequest hook. Claude Code runs it for every tool permission
// dialog, in the CLI, the desktop app and the IDE, alongside the dialog it
// shows. It mirrors the dialog into the dialogs directory of the binding that
// serves the session, else of the session's own session record, so the Buzz
// agent posts it in the session's own channel. Then it waits for the first answer: the owner's in Buzz, or the
// terminal's. A Buzz answer prints the decision; a terminal answer ends the
// hook with no output, so the terminal's choice stands. On any error it
// exits with no output and the terminal dialog decides. It never exits 2
// (not honored for this event). It reads no PATH and needs no AM_*: an
// app-launched session has neither.

// permissionHookMarker is the settings.json ownership signal of the
// PermissionRequest entry, distinct from the Stop hook's.
const permissionHookMarker = "AMQ_APPROVAL_HOOK=1 "

// permissionHookType is the hook event name in settings.json.
const permissionHookType = "PermissionRequest"

// harnessClaude names the harness in the request.
const harnessClaude = "claude"

// maxDialogText bounds the dialog body Buzz shows, in bytes cut on a UTF-8
// boundary; the options still apply to the whole call the terminal shows.
const maxDialogText = 3500

// permissionInput is the subset of the PermissionRequest stdin the hook
// reads.
type permissionInput struct {
	SessionID      string            `json:"session_id"`
	Cwd            string            `json:"cwd"`
	TranscriptPath string            `json:"transcript_path"`
	HookEventName  string            `json:"hook_event_name"`
	ToolName       string            `json:"tool_name"`
	ToolInput      json.RawMessage   `json:"tool_input"`
	Suggestions    []json.RawMessage `json:"permission_suggestions"`
	// AgentID names the subagent whose call asks; absent on the main
	// thread. A subagent's calls and results are only in its own
	// transcript.
	AgentID             string `json:"agent_id"`
	AgentTranscriptPath string `json:"agent_transcript_path"`
}

// callTranscript is the transcript that records the asking call and its
// result: the session's for the main thread, else the subagent's own.
func (in permissionInput) callTranscript() (string, error) {
	switch {
	case in.AgentID == "":
		return in.TranscriptPath, nil
	case in.AgentTranscriptPath != "":
		return in.AgentTranscriptPath, nil
	}
	return SubagentTranscript(in.TranscriptPath, in.SessionID, in.AgentID)
}

// syntheticModel is the model of an assistant line Claude Code writes
// itself, such as an API error; it can land while a dialog shows.
const syntheticModel = "<synthetic>"

// Option values the hook applies.
const (
	valueAllow        = "allow"
	valueAllowSession = "allow_session"
	valueDeny         = "deny"
)

// Seat is the session a dialog comes from, as the hook found it.
type Seat struct {
	Root, Handle string // AM_ROOT and AM_ME; empty outside AMQ
	Session      string // the hook's session_id
	// Holder is the Claude process the hook runs under (claudeAncestor);
	// zero when none was found.
	Holder launch.HolderIdentity
	Cwd    string // the hook's cwd, for a session with no registry entry
	// Transcript is the hook's transcript_path when it is absolute; "" else.
	Transcript string
}

// Binder writes the binding of a seat that no binding serves, as /amq-remote
// writes it, and returns its name. It refuses a session whose owner turned
// it off.
type Binder func(Seat) (string, error)

// permissionHook holds the hook's clock and pacing, so tests can drive it,
// and the writer of a new binding. It ends when Buzz or the terminal
// answers, when done closes, when the process that started it is gone
// (parentGone), or at the installed timeout, which Claude enforces too.
// It polls every poll, doubling the wait after each poll up to maxPoll
// (zero: no growth), so a dialog nobody answers costs little.
type permissionHook struct {
	now        func() time.Time
	poll       time.Duration
	maxPoll    time.Duration
	parentGone func() bool // nil: no parent check
	bind       Binder      // nil: a session no binding serves answers in the terminal
}

// RunPermissionHook is the hook body. done closes when the process gets
// TERM, INT or HUP: Claude sends TERM when the terminal rejects and at the
// configured timeout. bind writes the binding of a session no binding
// serves. It always returns 0.
func RunPermissionHook(stdin io.Reader, stdout io.Writer, done <-chan struct{}, bind Binder) int {
	h := permissionHook{now: time.Now, poll: 250 * time.Millisecond, maxPoll: 2 * time.Second, parentGone: parentWatch(), bind: bind}
	return h.run(stdin, stdout, done)
}

// parentWatch reports, on each call, whether the process that started the
// hook is gone: the hook was reparented, or that pid no longer runs. A
// Claude process killed without a signal (SIGKILL, a crash) sends the hook
// no TERM. A hook started already orphaned (ppid 1 or less) has no parent
// to watch: nil.
func parentWatch() func() bool {
	ppid := os.Getppid()
	if ppid <= 1 {
		return nil
	}
	return func() bool {
		if os.Getppid() != ppid {
			return true
		}
		alive, err := pidAlive(ppid)
		return err == nil && !alive
	}
}

func (h permissionHook) run(stdin io.Reader, stdout io.Writer, done <-chan struct{}) int {
	defer func() { _ = recover() }() // no decision, unconditionally
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return 0
	}
	var in permissionInput
	if json.Unmarshal(raw, &in) != nil || in.ToolName == "" {
		return 0
	}
	if in.HookEventName != "" && in.HookEventName != permissionHookType {
		return 0
	}
	name, err := h.dialogName(in)
	if err != nil {
		return 0
	}
	dir, err := dialog.DirFor(name)
	if err != nil {
		return 0
	}
	options, sessionRules := dialogOptions(in)
	opened := h.now()
	req := dialog.Request{
		ID: dialog.NewID(), Harness: harnessClaude, Session: in.SessionID, Tool: in.ToolName,
		Text: dialogText(in.ToolName, in.ToolInput), Options: options,
		OpenedAt: opened.UTC(), PID: os.Getpid(),
	}
	// Claude signals no terminal approve: the call's tool_result in the
	// transcript that records the call is the proof, so the watch starts
	// before the request is visible.
	watch := newToolWatch(in.callTranscript, in.AgentID != "", in.ToolName, in.ToolInput)
	if dialog.Open(dir, req) != nil {
		return 0
	}
	deadline := opened.Add(permissionHookTimeout * time.Second)
	wait := h.poll
	timer := time.NewTimer(wait)
	defer timer.Stop()
	resolve := func(outcome, label, by string) bool {
		won, err := dialog.Resolve(dir, dialog.Resolved{ID: req.ID, Outcome: outcome, Label: label, By: by, At: h.now().UTC()})
		return won && err == nil
	}
	for {
		phase, _, a, _, _ := dialog.Lookup(dir, req.ID)
		if phase == dialog.PhaseResolved {
			return 0
		}
		if phase == dialog.PhaseAnswered {
			opt, ok := chosen(options, a.Option)
			if !ok {
				// The answer fits no option: reject it, so the owner can
				// answer again, and keep waiting.
				_ = dialog.RejectAnswer(dir, req.ID)
			} else {
				outcome := dialog.OutcomeAllowed
				if opt.Deny {
					outcome = dialog.OutcomeDenied
				}
				// The first resolution decides: print only after winning it.
				if resolve(outcome, opt.Label, dialog.ByBuzz) {
					_ = writeDecision(stdout, opt.Value, sessionRules)
				}
				return 0
			}
		}
		if watch != nil && watch.answered() {
			resolve(dialog.OutcomeTerminal, "", dialog.ByTerminal)
			return 0
		}
		if h.parentGone != nil && h.parentGone() || !h.now().Before(deadline) {
			resolve(dialog.OutcomeExpired, "", dialog.ByTimeout)
			return 0
		}
		select {
		case <-done:
			resolve(dialog.OutcomeTerminal, "", dialog.ByTerminal)
			return 0
		case <-timer.C:
		}
		if wait < h.maxPoll {
			wait = min(2*wait, h.maxPoll)
		}
		timer.Reset(wait)
	}
}

// dialogName is where the dialog opens, in order: the binding that serves
// the session (its AMQ handle or its session id); a binding, not proven
// ended, held by exactly the Claude process the hook runs under, as it is
// (a session after /clear keeps the binding attach made); else a binding
// bind writes now, so the machine agent gives the session a channel. With
// none, "" and the terminal decides.
func (h permissionHook) dialogName(in permissionInput) (string, error) {
	seat := Seat{Root: os.Getenv("AM_ROOT"), Handle: os.Getenv("AM_ME"), Session: in.SessionID, Cwd: in.Cwd}
	if filepath.IsAbs(in.TranscriptPath) {
		seat.Transcript = filepath.Clean(in.TranscriptPath)
	}
	name, err := dialog.FindBinding(seat.Root, seat.Handle, seat.Session)
	if err != nil || name != "" {
		return name, err
	}
	seat.Holder = claudeAncestor(hookAncestors(), seat.Session)
	if seat.Holder.PID > 0 {
		if name, err := heldBinding(seat.Holder); err != nil || name != "" {
			return name, err
		}
	}
	if h.bind == nil {
		return "", errors.New("no binding writer")
	}
	return h.bind(seat)
}

// heldBinding is the binding, not proven ended, whose recorded holder is
// exactly holder; "" when none is.
func heldBinding(holder launch.HolderIdentity) (string, error) {
	all, err := binding.List()
	if errors.Is(err, binding.ErrNone) || errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, b := range all {
		if b.Holder != holder {
			continue
		}
		if state, _ := b.Liveness(); state != binding.Ended {
			return b.Name, nil
		}
	}
	return "", nil
}

// ancestor is one process the hook runs under, as hookAncestors read it.
type ancestor struct {
	holder launch.HolderIdentity // the identity the process walk verified
	reg    *Session              // its session registry entry; nil when it has none
	exe    string                // its executable; "" when it cannot be read
}

// hookAncestors reads the processes the hook runs under, nearest first: each
// one's session registry entry and executable.
func hookAncestors() []ancestor {
	var out []ancestor
	for _, h := range launch.Ancestors() {
		a := ancestor{holder: h}
		if s, err := SessionForPID(h.PID); err == nil {
			a.reg = &s
		}
		a.exe, _ = launch.ProcessExecutable(h.PID)
		out = append(out, a)
	}
	return out
}

// claudeAncestor is the Claude process among procs: the one whose session
// registry entry names session, else the nearest that is a Claude process
// at all, by a registry entry (Claude may not have rewritten it yet after
// /clear) or by its executable (a session that writes no entry: a child
// session, the desktop app). Nearest wins, so a child session is never
// taken for the Claude process it runs under. Zero when none is.
func claudeAncestor(procs []ancestor, session string) launch.HolderIdentity {
	for _, p := range procs {
		if p.reg != nil && p.reg.ID == session {
			return p.holder
		}
	}
	for _, p := range procs {
		if p.reg != nil || claudeExecutable(p.exe) {
			return p.holder
		}
	}
	return launch.HolderIdentity{}
}

// claudeExecutable reports whether path is a Claude Code executable: one
// named claude (the desktop app's .../claude.app/Contents/MacOS/claude, a
// Homebrew or npm install), or a version file of the native installer,
// .../claude/versions/<version>.
func claudeExecutable(path string) bool {
	if path == "" {
		return false
	}
	dir := filepath.Dir(path)
	return filepath.Base(path) == harnessClaude || filepath.Base(dir) == "versions" && filepath.Base(filepath.Dir(dir)) == harnessClaude
}

// dialogOptions are the choices Buzz offers, numbered as shown, and the
// permission updates "Allow this session" applies. Claude offers it when
// the dialog suggests updates (each echoed with destination session), and
// for an edit tool without suggestions (accept edits for the session). Its
// label says the scope in short (sessionScope).
func dialogOptions(in permissionInput) ([]dialog.Option, []json.RawMessage) {
	opts := []dialog.Option{{N: 1, Label: "Allow once", Value: valueAllow}}
	var rules []json.RawMessage
	for _, s := range in.Suggestions {
		if r, ok := sessionScoped(s); ok {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 && editTool(in.ToolName) {
		rules = []json.RawMessage{json.RawMessage(`{"type":"setMode","mode":"acceptEdits","destination":"session"}`)}
	}
	if len(rules) > 0 {
		opts = append(opts, dialog.Option{N: 2, Label: sessionScope(rules), Value: valueAllowSession})
	}
	return append(opts, dialog.Option{N: len(opts) + 1, Label: "Deny", Value: valueDeny, Deny: true}), rules
}

// sessionScope is the label of "Allow this session" with what it allows,
// each path as Claude gives it with only the home directory shortened to ~:
// "Allow this session: Bash(rm:*)", "Allow edits in ~/proj this session",
// "Stop allowing edits in ~/proj this session", "Accept edits this session".
func sessionScope(rules []json.RawMessage) string {
	home, _ := os.UserHomeDir()
	var parts, ruleParts []string
	for _, raw := range rules {
		var r struct {
			Type  string `json:"type"`
			Mode  string `json:"mode"`
			Rules []struct {
				ToolName    string `json:"toolName"`
				RuleContent string `json:"ruleContent"`
			} `json:"rules"`
			Directories []string `json:"directories"`
		}
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		dirs := make([]string, len(r.Directories))
		for i, d := range r.Directories {
			dirs[i] = homeShort(d, home)
		}
		switch r.Type {
		case "setMode":
			if r.Mode == "acceptEdits" {
				parts = append(parts, "Accept edits this session")
			} else if r.Mode != "" {
				parts = append(parts, "Use "+r.Mode+" mode this session")
			}
		case "addDirectories":
			if len(dirs) > 0 {
				parts = append(parts, "Allow edits in "+strings.Join(dirs, ", ")+" this session")
			}
		case "removeDirectories":
			if len(dirs) > 0 {
				parts = append(parts, "Stop allowing edits in "+strings.Join(dirs, ", ")+" this session")
			}
		default:
			for _, rule := range r.Rules {
				if rule.RuleContent == "" {
					ruleParts = append(ruleParts, rule.ToolName)
				} else {
					ruleParts = append(ruleParts, rule.ToolName+"("+homeShort(rule.RuleContent, home)+")")
				}
			}
		}
	}
	if len(ruleParts) > 0 {
		parts = append([]string{"Allow this session: " + strings.Join(ruleParts, ", ")}, parts...)
	}
	if len(parts) == 0 {
		return "Allow this session"
	}
	return truncateRunes(strings.Join(parts, "; "), maxOptionLabel)
}

// maxOptionLabel bounds an option label.
const maxOptionLabel = 200

// homeShort writes the home directory as ~ where a path token starts with
// it, also in Claude's "//<absolute path>" rule form: "//Users/me/notes/**"
// is "~/notes/**". A token starts at the start of s or after a space, '(',
// '=', ':' or a quote, and the home directory must end at '/' or at the end
// of s; a path that only contains it stays as it is. Both conditions are
// read in s as given, so a replacement never makes a new token start.
func homeShort(s, home string) string {
	if home == "" || home == string(filepath.Separator) {
		return s
	}
	home = filepath.Clean(home)
	var b strings.Builder
	done := 0 // s[:done] is written
	for from := 0; ; {
		i := strings.Index(s[from:], home)
		if i < 0 {
			b.WriteString(s[done:])
			return b.String()
		}
		start, end := from+i, from+i+len(home)
		if start > 0 && s[start-1] == '/' {
			start-- // the "//" rule form
		}
		if (start == 0 || strings.ContainsRune(" (=:'\"", rune(s[start-1]))) && (end == len(s) || s[end] == '/') {
			b.WriteString(s[done:start] + "~")
			done = end
		}
		from = end
	}
}

func editTool(name string) bool {
	switch name {
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return true
	}
	return false
}

// sessionScoped is a permission suggestion with its destination set to
// session, so an allow from Buzz never writes a settings file.
func sessionScoped(raw json.RawMessage) (json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return nil, false
	}
	m["destination"] = json.RawMessage(`"session"`)
	out, err := json.Marshal(m)
	return out, err == nil
}

func chosen(options []dialog.Option, n int) (dialog.Option, bool) {
	for _, o := range options {
		if o.N == n {
			return o, true
		}
	}
	return dialog.Option{}, false
}

// dialogText renders the call as the terminal dialog shows it: the command,
// the edit as a diff, the file, the URL, or the input as JSON.
func dialogText(tool string, input json.RawMessage) string {
	var in struct {
		Command      string `json:"command"`
		Description  string `json:"description"`
		FilePath     string `json:"file_path"`
		OldString    string `json:"old_string"`
		NewString    string `json:"new_string"`
		ReplaceAll   bool   `json:"replace_all"`
		Content      string `json:"content"`
		NotebookPath string `json:"notebook_path"`
		CellID       string `json:"cell_id"`
		EditMode     string `json:"edit_mode"`
		NewSource    string `json:"new_source"`
		URL          string `json:"url"`
		Prompt       string `json:"prompt"`
		Edits        []struct {
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		} `json:"edits"`
	}
	_ = json.Unmarshal(input, &in)
	var b strings.Builder
	switch {
	case tool == "Bash" && in.Command != "":
		b.WriteString("$ " + in.Command)
		if in.Description != "" {
			b.WriteString("\n(" + in.Description + ")")
		}
	case tool == "Edit" && in.FilePath != "":
		b.WriteString(in.FilePath + "\n")
		writeDiff(&b, in.OldString, in.NewString, in.ReplaceAll)
	case tool == "MultiEdit" && in.FilePath != "":
		b.WriteString(in.FilePath + "\n")
		for _, e := range in.Edits {
			writeDiff(&b, e.OldString, e.NewString, e.ReplaceAll)
		}
	case tool == "Write" && in.FilePath != "":
		b.WriteString(in.FilePath + "\n" + in.Content)
	case tool == "NotebookEdit" && in.NotebookPath != "":
		b.WriteString(in.NotebookPath)
		if in.CellID != "" {
			b.WriteString(" cell " + in.CellID)
		}
		if in.EditMode != "" {
			b.WriteString(" (" + in.EditMode + ")")
		}
		b.WriteString("\n" + in.NewSource)
	case tool == "WebFetch" && in.URL != "":
		b.WriteString(in.URL)
		if in.Prompt != "" {
			b.WriteString("\n" + in.Prompt)
		}
	default:
		if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
			if server, name, ok := strings.Cut(rest, "__"); ok {
				b.WriteString(server + "/" + name + "\n")
			}
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, input, "", "  ") == nil {
			b.Write(pretty.Bytes())
		} else {
			b.Write(input)
		}
	}
	text := strings.TrimRight(b.String(), "\n")
	if t, cut := protocol.TruncateText(text, maxDialogText); cut {
		text = t + "\n… full text in the terminal"
	}
	return text
}

// writeDiff writes old and new as removed and added lines.
func writeDiff(b *strings.Builder, old, new string, all bool) {
	if all {
		b.WriteString("(every occurrence)\n")
	}
	for _, l := range strings.Split(old, "\n") {
		b.WriteString("- " + l + "\n")
	}
	for _, l := range strings.Split(new, "\n") {
		b.WriteString("+ " + l + "\n")
	}
}

// truncateRunes cuts s to at most n runes, saying where the rest is.
func truncateRunes(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "\n… full text in the terminal"
}

// writeDecision prints the hook's decision: allow (with the session updates for valueAllowSession),
// or deny with a message and no interrupt, so the turn continues.
func writeDecision(stdout io.Writer, value string, sessionRules []json.RawMessage) error {
	type decision struct {
		Behavior           string            `json:"behavior"`
		Message            string            `json:"message,omitempty"`
		UpdatedPermissions []json.RawMessage `json:"updatedPermissions,omitempty"`
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName string   `json:"hookEventName"`
			Decision      decision `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = permissionHookType
	switch value {
	case valueAllow:
		out.HookSpecificOutput.Decision = decision{Behavior: "allow"}
	case valueAllowSession:
		out.HookSpecificOutput.Decision = decision{Behavior: "allow", UpdatedPermissions: sessionRules}
	default:
		out.HookSpecificOutput.Decision = decision{Behavior: "deny", Message: "Denied from Buzz"}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	n, err := stdout.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if f, ok := stdout.(interface{ Flush() error }); ok && err == nil {
		err = f.Flush()
	}
	return err
}

// toolWatch follows the transcript of the chain that asks (the main thread,
// or a subagent in its own transcript) for proof that the dialog is gone:
//   - the tool_result of the call: a tool_use with the same name and input
//     that had no result when the hook started, or that appears later;
//   - the chain moved on: an assistant message the chain had not written
//     when the dialog opened. While a dialog blocks a call the chain writes
//     none, because the model waits for the whole tool batch; a line Claude
//     Code writes itself (syntheticModel, an API error) proves nothing;
//   - an interrupt of the chain, which Esc and a reject write.
//
// It locates the transcript (locate) until it is found; a transcript first
// found after the hook started holds only lines written since, so its first
// read can answer a result, and the chain proofs start at the asking call.
type toolWatch struct {
	locate           func() (string, error)
	path, name, hash string
	// ownChain: the transcript is the asking subagent's own, so its
	// sidechain lines are the chain; otherwise sidechain lines are other
	// chains'.
	ownChain bool
	// armed: lines read from now on were written after the dialog opened.
	// seen holds the assistant message ids read before them.
	armed bool
	seen  map[string]bool
	// off is the next byte to read; skip drops the first line of a tail
	// read, which may start mid-line.
	off  int64
	skip bool
	// primed: the first read is done, so a result read after it answers
	// the dialog; one read by the first read settled an earlier call.
	primed  bool
	pending map[string]bool
}

// transcriptTail is how far back the first read reaches: the call's
// tool_use is among the newest lines.
const transcriptTail = 4 << 20

func newToolWatch(locate func() (string, error), ownChain bool, name string, input json.RawMessage) *toolWatch {
	hash, ok := actionHash(name, input)
	if !ok {
		return nil
	}
	w := &toolWatch{locate: locate, ownChain: ownChain, name: name, hash: hash, pending: map[string]bool{}, seen: map[string]bool{}}
	if w.found() {
		w.read()
		w.armed = true
	}
	w.primed = true
	return w
}

// found reports whether the transcript is located, locating it once.
func (w *toolWatch) found() bool {
	if w.path != "" {
		return true
	}
	path, err := w.locate()
	if err != nil || !filepath.IsAbs(path) {
		return false
	}
	w.path = path
	if fi, err := os.Stat(path); err == nil && fi.Size() > transcriptTail {
		w.off, w.skip = fi.Size()-transcriptTail, true
	}
	return true
}

// answered reads the lines written since the last read and reports whether
// a pending call got its tool_result.
func (w *toolWatch) answered() bool { return w.found() && w.read() }

func (w *toolWatch) read() bool {
	f, err := os.Open(w.path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(w.off, io.SeekStart); err != nil {
		return false
	}
	rd := bufio.NewReaderSize(f, 64<<10)
	answered := false
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return answered // a partial last line is read again next time
		}
		w.off += int64(len(line))
		if w.skip {
			w.skip = false
			continue
		}
		e, ok := parseTranscriptLine(strings.TrimSuffix(line, "\n"))
		if !ok {
			continue
		}
		if chain := !e.Sidechain || w.ownChain; chain {
			if e.Type == "assistant" && e.MessageID != "" && !e.APIError && e.Model != syntheticModel {
				answered = answered || w.armed && !w.seen[e.MessageID]
				w.seen[e.MessageID] = true
			}
			answered = answered || w.armed && e.Type == "user" && interrupted(e)
		}
		for _, u := range e.ToolUses {
			if u.Name == w.name && u.Hash == w.hash {
				w.pending[u.ID] = true
				w.armed = w.armed || w.primed
			}
		}
		for _, id := range e.ToolResults {
			if w.pending[id] {
				delete(w.pending, id)
				answered = answered || w.primed
			}
		}
	}
}

// interrupted reports a user line that interrupts its chain.
func interrupted(e transcriptEntry) bool {
	for _, b := range e.Blocks {
		if b.Type == "text" && strings.HasPrefix(b.Text, interruptPrefix) {
			return true
		}
	}
	return false
}

// actionHash identifies one exact tool call: SHA-256 over the tool name and
// the canonical tool input. The hook hashes its stdin tool_input and the
// transcript's tool_use input with this same function, so both agree byte
// for byte.
func actionHash(toolName string, input json.RawMessage) (string, bool) {
	canon, ok := canonicalJSON(input)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte("amq-remote/claude/action\x00" + toolName + "\x00" + string(canon)))
	return "sha256:" + hex.EncodeToString(sum[:]), true
}

// canonicalJSON re-encodes a JSON value with sorted object keys and number
// literals kept as written.
func canonicalJSON(raw json.RawMessage) ([]byte, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("null")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	return out, err == nil
}
