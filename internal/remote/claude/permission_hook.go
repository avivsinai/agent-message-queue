package claude

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
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

// DefaultPermissionWait is the hook's own deadline. The installed timeout is
// this plus PermissionHookGrace, so the hook normally ends first.
const (
	DefaultPermissionWait = 600 * time.Second
	PermissionHookGrace   = 30 * time.Second
)

// harnessClaude names the harness in the request.
const harnessClaude = "claude"

// maxDialogText bounds the dialog body Buzz shows; the options still apply
// to the whole call the terminal shows.
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
}

// Option values the hook applies.
const (
	valueAllow        = "allow"
	valueAllowSession = "allow_session"
	valueDeny         = "deny"
)

// permissionHook holds the hook's clock and pacing, so tests can drive it,
// and the writer of an AMQ session's mailbox binding.
type permissionHook struct {
	now  func() time.Time
	wait time.Duration
	poll time.Duration
	// mailbox writes the mailbox binding of handle in root as attach
	// --self does and returns its name; nil when the caller has none.
	mailbox func(root, handle string) (string, error)
}

// RunPermissionHook is the hook body. done closes when the process gets
// TERM, INT or HUP: Claude sends TERM when the terminal rejects and at the
// configured timeout. It always returns 0.
// mailbox writes the mailbox binding of an AMQ session that no binding
// serves (see permissionHook).
func RunPermissionHook(stdin io.Reader, stdout io.Writer, done <-chan struct{}, wait time.Duration, mailbox func(root, handle string) (string, error)) int {
	if wait <= 0 {
		wait = DefaultPermissionWait
	}
	h := permissionHook{now: time.Now, wait: wait, poll: 250 * time.Millisecond, mailbox: mailbox}
	return h.run(stdin, stdout, done)
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
	// transcript is the proof, so the watch starts before the request is
	// visible.
	watch := newToolWatch(in.TranscriptPath, in.ToolName, in.ToolInput)
	if dialog.Open(dir, req) != nil {
		return 0
	}
	deadline := opened.Add(h.wait)
	ticker := time.NewTicker(h.poll)
	defer ticker.Stop()
	resolve := func(outcome, label, by string) bool {
		won, err := dialog.Resolve(dir, dialog.Resolved{ID: req.ID, Outcome: outcome, Label: label, By: by, At: h.now().UTC()})
		return won && err == nil
	}
	for {
		if _, closed, _ := dialog.ReadResolved(dir, req.ID); closed {
			return 0
		}
		if a, ok, _ := dialog.ReadAnswer(dir, req.ID); ok {
			if opt, ok := chosen(options, a.Option); ok {
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
		if !h.now().Before(deadline) {
			resolve(dialog.OutcomeExpired, "", dialog.ByTimeout)
			return 0
		}
		select {
		case <-done:
			resolve(dialog.OutcomeTerminal, "", dialog.ByTerminal)
			return 0
		case <-ticker.C:
		}
	}
}

// dialogName is where the dialog opens: the binding that serves the session
// (its AMQ handle or its session id), else the session's own binding,
// written now as /amq-remote writes it, so the machine agent gives it a
// channel like any binding: the mailbox binding of an AMQ session (AM_ROOT
// and AM_ME set), else the native binding attach --self --native writes.
// After /clear the same Claude process writes the same native binding again
// with its new session id. With no Claude process found, "" and the
// terminal decides.
func (h permissionHook) dialogName(in permissionInput) (string, error) {
	root, handle := os.Getenv("AM_ROOT"), os.Getenv("AM_ME")
	name, err := dialog.FindBinding(root, handle, in.SessionID)
	if err != nil || name != "" {
		return name, err
	}
	if root != "" && handle != "" {
		if h.mailbox == nil {
			return "", errors.New("no mailbox binding writer")
		}
		return h.mailbox(root, handle)
	}
	pid, reg, ok := claudeAncestor(in.SessionID)
	if !ok {
		return "", errors.New("no Claude process above the hook")
	}
	holder, err := launch.HolderOf(pid)
	if err != nil {
		return "", err
	}
	if root == "" {
		if root, err = binding.EnsureMachineRoot(); err != nil {
			return "", err
		}
	}
	return writeNativeBinding(root, pid, holder, cmp.Or(reg.Cwd, in.Cwd), reg.Name, in.SessionID)
}

// writeNativeBinding writes the native binding of the Claude session that
// process pid runs, with the target, label, identity and name attach --self
// --native gives it, through binding.WriteSession: the same session (same
// identity and holder) gets its name back with the binding rewritten.
func writeNativeBinding(root string, pid int, holder launch.HolderIdentity, cwd, display, session string) (string, error) {
	target := "claude:" + strconv.Itoa(pid)
	machine := binding.Machine()
	b := binding.Binding{Root: root, Target: target, NativeSession: session, Display: cmp.Or(display, harnessClaude+" "+target),
		Name: binding.SanitizeName(target), Harness: harnessClaude, Holder: holder, Machine: machine}
	place := cmp.Or(b.Display, target)
	if cwd != "" {
		place = filepath.Base(cwd)
		b.Identity = binding.NativeIdentity(harnessClaude, root, cwd)
		b.Name = binding.SanitizeName(harnessClaude + "-" + place)
	}
	b.Label = binding.SessionLabel(harnessClaude, place, machine)
	name, _, err := binding.WriteSession(b, b.Name, binding.Binding.Liveness)
	return name, err
}

// claudeAncestor is the Claude process the hook runs under, nearest first:
// the first ancestor whose session registry entry names session, else the
// nearest whose executable is claude or that has a registry entry. ok is
// false when no ancestor is one.
func claudeAncestor(session string) (pid int, reg Session, ok bool) {
	fallback, found := 0, Session{}
	for _, h := range launch.Ancestors() {
		s, err := SessionForPID(h.PID)
		if err == nil && s.ID == session {
			return h.PID, s, true
		}
		if fallback != 0 {
			continue
		}
		if err == nil {
			fallback, found = h.PID, s
		} else if exe, err := launch.ProcessExecutable(h.PID); err == nil && filepath.Base(exe) == harnessClaude {
			fallback = h.PID
		}
	}
	return fallback, found, fallback != 0
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
// of s; a path that only contains it stays as it is.
func homeShort(s, home string) string {
	if home == "" || home == string(filepath.Separator) {
		return s
	}
	home = filepath.Clean(home)
	var b strings.Builder
	for {
		i := strings.Index(s, home)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		start, end := i, i+len(home)
		if start > 0 && s[start-1] == '/' {
			start-- // the "//" rule form
		}
		if (start == 0 || strings.ContainsRune(" (=:'\"", rune(s[start-1]))) && (end == len(s) || s[end] == '/') {
			b.WriteString(s[:start] + "~")
		} else {
			b.WriteString(s[:end])
		}
		s = s[end:]
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
	return truncateRunes(b.String(), maxDialogText)
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

// toolWatch follows a Claude transcript for the tool_result of the call a
// dialog is about: a tool_use with the same name and input that had no
// result when the hook started, or that appears later.
type toolWatch struct {
	path, name, hash string
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

func newToolWatch(path, name string, input json.RawMessage) *toolWatch {
	hash, ok := actionHash(name, input)
	if !ok || !filepath.IsAbs(path) {
		return nil
	}
	w := &toolWatch{path: path, name: name, hash: hash, pending: map[string]bool{}}
	if fi, err := os.Stat(path); err == nil && fi.Size() > transcriptTail {
		w.off, w.skip = fi.Size()-transcriptTail, true
	}
	w.read()
	w.primed = true
	return w
}

// answered reads the lines written since the last read and reports whether
// a pending call got its tool_result.
func (w *toolWatch) answered() bool { return w.read() }

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
		for _, u := range e.ToolUses {
			if u.Name == w.name && u.Hash == w.hash {
				w.pending[u.ID] = true
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
