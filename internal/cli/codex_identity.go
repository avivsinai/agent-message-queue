package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

// identityEnvKeys are the variables that make up an AMQ participant
// identity. Presence of any one, even empty, is an explicit context.
var identityEnvKeys = []string{envRoot, envBaseRoot, envSession, envMe, envRootID, envBaseRootID}

// adoptCodexThreadIdentity gives an amq command run inside a Codex thread the
// identity amq coop exec recorded for that thread (agent-message-queue-611.61).
// On Codex 0.160 tool commands run in the shared daemon's environment and see
// none of the AM_* variables coop exec set, only CODEX_THREAD_ID. It acts only
// when every identity variable is absent; the recorded identity is fully
// validated before any of it becomes visible, and then the usual pin checks
// run on it. A thread AMQ never recorded keeps today's behavior. A recorded
// but unusable identity (damaged, foreign CODEX_HOME, replaced by a later
// coop exec, or a root that changed) is a context mismatch for every command,
// never a fallback to default resolution; setting the identity variables
// explicitly is the way out.
func adoptCodexThreadIdentity() error {
	thread, ok := os.LookupEnv("CODEX_THREAD_ID")
	if !ok || strings.TrimSpace(thread) == "" {
		return nil
	}
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			return nil
		}
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return ContextMismatchError("cannot resolve CODEX_HOME for this Codex thread's AMQ identity: %v", err)
	}
	rec, err := codexidentity.Lookup(strings.TrimSpace(thread), absPath(filepath.Clean(codexHome)))
	if errors.Is(err, codexidentity.ErrNone) {
		return nil
	}
	if err != nil {
		return ContextMismatchError("this Codex thread's AMQ identity cannot be used: %v; start the session again with amq coop exec, or set AM_ROOT and AM_ME explicitly", err)
	}
	if err := validateRecordedIdentity(rec); err != nil {
		return err
	}
	values := map[string]string{
		envRoot: rec.Root, envBaseRoot: rec.BaseRoot, envSession: rec.Session,
		envMe: rec.Me, envRootID: rec.RootID, envBaseRootID: rec.BaseRootID,
	}
	for _, key := range identityEnvKeys {
		if err := os.Setenv(key, values[key]); err != nil {
			for _, k := range identityEnvKeys {
				_ = os.Unsetenv(k)
			}
			return ContextMismatchError("cannot adopt this Codex thread's AMQ identity: %v", err)
		}
	}
	return nil
}

// validateRecordedIdentity applies the checks an inherited pin gets, before
// the identity is published: a valid handle and session, absolute roots in
// the right relation, well-formed tokens, and trees that are still the ones
// coop exec pinned.
func validateRecordedIdentity(rec codexidentity.Record) error {
	if handle, err := normalizeHandle(rec.Me); err != nil || handle != rec.Me {
		return ContextMismatchError("this Codex thread's recorded AMQ handle %q is invalid", rec.Me)
	}
	want := rec.BaseRoot
	if rec.Session != "" {
		if err := validateSessionName(rec.Session); err != nil {
			return ContextMismatchError("this Codex thread's recorded AMQ session %q is invalid", rec.Session)
		}
		want = filepath.Join(rec.BaseRoot, rec.Session)
	}
	if filepath.Clean(rec.Root) != filepath.Clean(want) {
		return ContextMismatchError("this Codex thread's recorded AMQ root %s is not %s", rec.Root, want)
	}
	if !validTreeIdentityToken(rec.RootID) || !validTreeIdentityToken(rec.BaseRootID) {
		return ContextMismatchError("this Codex thread's recorded AMQ identity tokens are malformed")
	}
	return verifyRootUnderBase(rec.BaseRoot, rec.BaseRootID, rec.Session, rec.Root, rec.RootID)
}

// codexThreadIdentityRecorder returns the function coop exec calls once, for
// the Codex thread the TUI will resume: it records the identity in env for
// that thread. Without complete root identity tokens it returns nil, so no
// thread is handed to a TUI with an identity AMQ could not record.
func codexThreadIdentityRecorder(env []string) func(thread string) error {
	get := func(key string) string {
		prefix := key + "="
		for _, entry := range env {
			if v, ok := strings.CutPrefix(entry, prefix); ok {
				return v
			}
		}
		return ""
	}
	rec := codexidentity.Record{
		Root: get(envRoot), BaseRoot: get(envBaseRoot), Session: get(envSession),
		Me: get(envMe), RootID: get(envRootID), BaseRootID: get(envBaseRootID),
	}
	if rec.Root == "" || rec.Me == "" || rec.RootID == "" || rec.BaseRootID == "" {
		return nil
	}
	return func(thread string) error {
		codexHome, err := codexHomeDir()
		if err != nil {
			return err
		}
		r := rec
		r.Thread = thread
		r.CodexHome = absPath(filepath.Clean(codexHome))
		return codexidentity.Publish(r)
	}
}

// codexResume is a parsed codex resume [OPTIONS] [SESSION_ID] [PROMPT]
// launch (codex-cli 0.160), where AMQ's own session resume puts its options
// first and the id last.
type codexResume struct {
	// start is the index of resume in args; opts are the options after it,
	// less --last, --all and --include-non-interactive.
	start int
	opts  []string
	pos   []codexPositional
	// last, all and includeNonInteractive are the selection-only flags.
	last, all, includeNonInteractive bool
	// unparsed is an option AMQ does not know (or -i), or more than two
	// positionals; parsing stops at such an option.
	unparsed bool
}

type codexPositional struct {
	value     string
	afterDash bool
}

// parseCodexResume parses args; ok is false for a launch that is not a resume.
func parseCodexResume(args []string) (r codexResume, ok bool) {
	// resume is the first positional; options may stand before it.
	r.start = -1
	for i := 0; i < len(args) && r.start < 0; i++ {
		flag, _, inline := strings.Cut(args[i], "=")
		switch {
		case args[i] == "--":
			return r, false
		case !strings.HasPrefix(args[i], "-"):
			if args[i] != "resume" {
				return r, false
			}
			r.start = i
		case codexValueOptions[flag] && !inline:
			i++
		}
	}
	if r.start < 0 {
		return r, false
	}
	dash := false
	for i := r.start + 1; i < len(args); i++ {
		arg := args[i]
		if dash || arg != "--" && !strings.HasPrefix(arg, "-") {
			r.pos = append(r.pos, codexPositional{arg, dash})
			continue
		}
		flag, _, inline := strings.Cut(arg, "=")
		switch {
		case arg == "--":
			dash = true
		case arg == "--last":
			r.last = true
		case arg == "--all":
			r.all = true
		case arg == "--include-non-interactive":
			r.includeNonInteractive = true
		case codexFlagOptions[flag] || codexEmbeddedFlags[flag]:
			r.opts = append(r.opts, arg)
		case codexValueOptions[flag] && (inline || i+1 < len(args)):
			r.opts = append(r.opts, arg)
			if !inline {
				i++
				r.opts = append(r.opts, args[i])
			}
		default:
			// With --last anywhere Codex takes the most recent session and
			// reads a leading id as the prompt.
			r.unparsed = true
			r.last = r.last || slices.Contains(args[i:], "--last")
			return r, true
		}
	}
	r.unparsed = r.unparsed || len(r.pos) > 2
	return r, true
}

// selection applies codex-cli 0.160's argument mapping (cli/src/main.rs
// finalize_resume_interactive): with --last and one positional, it is the
// prompt; otherwise the first positional selects the thread and wins over
// --last. last reports whether the most recent thread is selected.
func (r codexResume) selection() (selector, prompt *codexPositional, last bool) {
	switch {
	case r.last && len(r.pos) == 1:
		return nil, &r.pos[0], true
	case len(r.pos) == 0:
		return nil, nil, r.last
	case len(r.pos) == 1:
		return &r.pos[0], nil, false
	}
	return &r.pos[0], &r.pos[1], false
}

// codexResumeSelection reads the thread a Codex resume launch selects.
// thread is the id when the first positional is a thread id; selected is
// false for a launch that is not a resume, and unresolved is true for a
// resume that names no thread id (--last, a name, the picker) or that AMQ
// cannot parse.
func codexResumeSelection(args []string) (thread string, selected, unresolved bool) {
	r, ok := parseCodexResume(args)
	if !ok {
		return "", false, false
	}
	if !r.last && len(r.pos) > 0 && codexidentity.ValidThread(r.pos[0].value) {
		return r.pos[0].value, true, false
	}
	return "", true, true
}

// codexResumeRefusal stops a resume launch whose thread AMQ cannot select.
func codexResumeRefusal(format string, args ...any) error {
	return ContextMismatchError("cannot bind the requested AMQ identity before this Codex resume launch: %s. Pass a concrete thread id: amq coop exec ... codex -- resume <thread-id>, or use amq session resume. Codex was not started", fmt.Sprintf(format, args...))
}

// resolveCodexResumeLaunch turns `codex resume --last` and `codex resume
// <name>` on the Codex daemon into `codex resume <thread-id>`, so the thread
// coop exec binds is the thread the TUI opens (agent-message-queue-611.64).
// AMQ selects once, with the TUI's own daemon requests, and the id travels in
// argv; recordResumedCodexThread then records it. AMQ ports no Codex
// heuristic: where Codex's cwd or provider choice could differ from AMQ's
// (-C, a symlinked cwd, linked worktrees, a profile's provider) --last or a
// name refuses. Every other resume passes through unchanged, and
// recordResumedCodexThread refuses those it cannot bind.
func resolveCodexResumeLaunch(binaryPath string, args []string) ([]string, error) {
	if launch.ProviderForExecutable(binaryPath) != launch.CodexProvider {
		return args, nil
	}
	r, ok := parseCodexResume(args)
	if !ok || r.unparsed {
		return args, nil
	}
	selector, prompt, last := r.selection()
	if selector == nil && !last {
		return args, nil // the picker
	}
	if selector != nil && codexidentity.ValidThread(selector.value) {
		if !r.last {
			return args, nil
		}
		return rewriteCodexResume(args, r, selector.value, prompt), nil
	}
	if selector != nil && codex.ParsesAsUUID(selector.value) {
		return nil, codexResumeRefusal("pass the thread id %s in lowercase hyphenated form", selector.value)
	}
	// syscall.Getwd, not os.Getwd: the TUI's cwd is getcwd, not $PWD.
	wd, err := syscall.Getwd()
	if err != nil {
		return nil, codexResumeRefusal("cannot read the working directory: %v", err)
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return nil, codexResumeRefusal("cannot resolve CODEX_HOME: %v", err)
	}
	opts := append(slices.Clone(args[:r.start]), r.opts...)
	backend, features := codexLaunchBackend(binaryPath, opts, wd, codexHome)
	switch backend {
	case codexEmbedded:
		return nil, codexResumeRefusal("this Codex runs its own app-server, where AMQ cannot select the thread")
	case codexBackendUnknown:
		return nil, codexResumeRefusal("AMQ cannot tell whether this Codex runs on its shared daemon")
	}
	sock, err := codex.ControlSocket(codexHome)
	if err != nil {
		return nil, codexResumeRefusal("no Codex daemon is running")
	}
	query := codex.ResumeQuery{ConfigCwd: codexLaunchDir(opts, wd), IncludeNonInteractive: r.includeNonInteractive, CodexHome: codexHome}
	if selector != nil {
		query.Name = selector.value
	} else if !r.all {
		if reason := codexLastCwdChanges(opts, wd, features); reason != "" {
			return nil, codexResumeRefusal("%s changes which thread Codex calls last", reason)
		}
		query.Cwd = wd
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexDaemonNamingTimeout)
	defer cancel()
	thread, err := codex.ResolveResumeThread(ctx, sock, query)
	switch {
	case errors.Is(err, codex.ErrNoResumeThread) && selector != nil:
		return nil, codexResumeRefusal("no saved Codex session is named %q", selector.value)
	case errors.Is(err, codex.ErrNoResumeThread):
		return nil, codexResumeRefusal("no Codex session to resume for %s; start without resume", wd)
	case err != nil:
		return nil, codexResumeRefusal("Codex daemon: %v", err)
	case !codexidentity.ValidThread(thread):
		return nil, codexResumeRefusal("Codex daemon returned thread id %q", thread)
	}
	return rewriteCodexResume(args, r, thread, prompt), nil
}

// codexLastCwdChanges names what makes the TUI filter --last by another cwd
// than wd, or "": -C (canonicalized by Codex), a symlinked cwd, or linked
// worktrees while the worktrees feature is on (tui/src/resume_picker.rs
// repository_cwd_filter).
func codexLastCwdChanges(opts []string, wd string, features map[string]bool) string {
	worktrees := features["worktrees"]
	for i := 0; i < len(opts); i++ {
		flag, value, inline := strings.Cut(opts[i], "=")
		if !codexValueOptions[flag] {
			continue
		}
		if !inline && i+1 < len(opts) {
			i++
			value = opts[i]
		}
		key, raw, _ := strings.Cut(value, "=")
		switch {
		case flag == "-C" || flag == "--cd":
			return "-C"
		case (flag == "--enable" || flag == "--disable") && value == "worktrees":
			worktrees = flag == "--enable"
		case (flag == "-c" || flag == "--config") && strings.TrimSpace(key) == "features.worktrees":
			worktrees = strings.TrimSpace(raw) == "true"
		}
	}
	if real, err := filepath.EvalSymlinks(wd); err != nil || real != wd {
		return "a symlinked working directory"
	}
	if worktrees && codexHasLinkedWorktrees(wd) {
		return "a repository with linked worktrees"
	}
	return ""
}

// codexHasLinkedWorktrees reports whether dir's checkout is a linked
// worktree or has one.
func codexHasLinkedWorktrees(dir string) bool {
	root := codexNearestGitAncestor(dir)
	if root == "" {
		return false
	}
	if info, err := os.Lstat(filepath.Join(root, ".git")); err != nil || !info.IsDir() {
		return true
	}
	entries, err := os.ReadDir(filepath.Join(root, ".git", "worktrees"))
	return len(entries) > 0 || err != nil && !errors.Is(err, fs.ErrNotExist)
}

// rewriteCodexResume returns args resuming thread: the options before and
// after resume, the id, and the prompt (after -- if it was).
func rewriteCodexResume(args []string, r codexResume, thread string, prompt *codexPositional) []string {
	out := append(slices.Clone(args[:r.start+1]), r.opts...)
	out = append(out, thread)
	if prompt != nil {
		if prompt.afterDash {
			out = append(out, "--")
		}
		out = append(out, prompt.value)
	}
	return out
}

// recordResumedCodexThread records the identity for the Codex thread a
// resume launch selects by id: the one AMQ named and created, or one the
// user resumes (codex resume <id>, or AMQ session resume's
// codex resume [options] <id>). Its tool commands may run on the daemon. If
// the identity cannot be recorded the launch stops: a thread is never
// resumed without it.
func recordResumedCodexThread(binaryPath string, args []string, record func(thread string) error) error {
	if launch.ProviderForExecutable(binaryPath) != launch.CodexProvider {
		return nil
	}
	thread, selected, unresolved := codexResumeSelection(args)
	if !selected {
		return nil
	}
	// Codex would choose the thread itself (--last, a name, the picker), and
	// that thread may carry another session's or handle's identity: AMQ
	// starts nothing it cannot bind first.
	if unresolved {
		return ContextMismatchError("cannot bind the requested AMQ identity before this Codex resume launch: the selection (--last, a session name, the picker, or an option AMQ does not parse) could resume a thread recorded for another session or handle. Pass a concrete thread id: amq coop exec ... codex -- resume <thread-id>, or use amq session resume. Codex was not started")
	}
	if record == nil {
		return ContextMismatchError("AMQ cannot record this session's identity for Codex thread %s", thread)
	}
	if err := record(thread); err != nil {
		return ContextMismatchError("AMQ could not record this session's identity for Codex thread %s: %v", thread, err)
	}
	return nil
}
