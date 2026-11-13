//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/claudeidentity"
	"github.com/avivsinai/agent-message-queue/internal/cli/wakemutation"
	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/hookfile"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/writerlock"
)

// amq coop join binds the session it runs in to one handle, so a session
// amq coop exec did not start (a chat of the Claude Code or Codex desktop
// app) is one agent. A Claude Code session is bound by its session key (see
// claudeSessionKey), a Codex thread by its thread id: two sessions in one
// project join with two handles. Wake policy stays with the handle, in the
// wake settings.

// coopJoinCanRun reports whether amq coop join would get past its checks of
// the session this command runs in: one Claude Code session with a session
// record among this process's ancestors, or one Codex thread whose writer
// lock is held, and no AM_* identity variable. A hint names the join only
// then.
func coopJoinCanRun() bool {
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			return false
		}
	}
	s, err := sessionHere("join")
	return err == nil && s.runs() == nil
}

// joinSession is the harness-specific part of amq coop join and leave for
// the one session the command runs in.
type joinSession struct {
	harness string // harness.Claude or harness.Codex
	key     string // the Claude session key or the Codex thread id
	// kind names the session in sentences ("Claude session"), and place
	// where its user runs a command ("session", "chat").
	kind, place string
	// whose names the session in identity errors.
	whose string
	// envHint says where an AM_* variable of this harness usually comes
	// from.
	envHint string
	// runs refuses a session that a later join could not tell from one that
	// ended.
	runs func() error
	// held returns the handle the session holds, ok false when it holds
	// none, and an error when its binding cannot be read.
	held func() (handle string, ok bool, err error)
	// defaultHandle is the handle of a bare join by a session that holds
	// none, and the sentence that says where it came from.
	defaultHandle func() (handle, chosenBy string)
	// bind binds the session to id. held is the session that holds the
	// handle and still runs, when it refused.
	bind func(id launch.Identity) (replaced, held string, err error)
	// holder returns the session of this harness that holds a handle.
	holder func(root, rootID, me string) (string, bool, error)
	// other returns the session of the other harness that holds a handle,
	// and names that harness's kind of session.
	other     func(root, rootID, me string) (string, bool, error)
	otherKind string
	release   func() (me, root string, freed bool, err error)
	doorbell  func() string
}

// coopSessionHarness says which harness the command runs in: a Claude Code
// session (CLAUDE_CODE_SESSION_ID), a Codex thread (CODEX_THREAD_ID), or
// neither. Both present is refused: the command cannot tell which one it
// runs in.
func coopSessionHarness(verb string) (string, error) {
	_, _, claudeOK := claudeSessionID()
	_, codexOK := codexThreadHere()
	switch {
	case claudeOK && codexOK:
		return "", ActionRequiredError("coop %s cannot tell which session it runs in: this shell has both a Claude Code session (%s) and a Codex thread (CODEX_THREAD_ID); run it in a shell that has only one", verb, envClaudeSession)
	case claudeOK:
		return harness.Claude, nil
	case codexOK:
		return harness.Codex, nil
	}
	return "", ActionRequiredError("coop %s must run inside a Claude Code session or a Codex thread (neither %s nor CODEX_THREAD_ID is set to an id)", verb, envClaudeSession)
}

// codexThreadHere returns the Codex thread this command runs in, from
// CODEX_THREAD_ID, or ok false outside a Codex thread.
func codexThreadHere() (string, bool) {
	thread := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
	return thread, codexidentity.ValidThread(thread)
}

// sessionHere returns the session amq coop join or leave (verb) runs in.
func sessionHere(verb string) (*joinSession, error) {
	h, err := coopSessionHarness(verb)
	if err != nil {
		return nil, err
	}
	if h == harness.Codex {
		thread, _ := codexThreadHere()
		return codexJoinSession(thread)
	}
	return claudeJoinSession()
}

func claudeJoinSession() (*joinSession, error) {
	sessionID, configDir, _ := claudeSessionID()
	record, recordOK := ancestorClaudeSessionRecord(configDir, sessionID)
	key := sessionID
	if recordOK {
		key = claudeRecordKey(record)
	}
	return &joinSession{
		harness: harness.Claude, key: key,
		kind: "Claude session", place: "session", whose: "this Claude session's",
		envHint: "for example from the Claude settings env block",
		// A later join tells a holder that runs from one that ended by
		// this record, so a session without one cannot hold a handle.
		runs: func() error {
			if !recordOK {
				return ActionRequiredError("coop join found no Claude Code session record for session %s (%s) among the processes this command runs under; run it through the shell tool of that session, on a current Claude Code", sessionID, filepath.Join(configDir, "sessions"))
			}
			return nil
		},
		held: func() (string, bool, error) {
			rec, err := claudeidentity.Lookup(key, configDir)
			if errors.Is(err, claudeidentity.ErrNone) {
				return "", false, nil
			}
			return rec.Me, err == nil, err
		},
		defaultHandle: func() (string, string) { return claudeDefaultHandle(record) },
		bind: func(id launch.Identity) (string, string, error) {
			replaced, err := claudeidentity.Bind(claudeidentity.Record{ClaudeSession: key, ConfigDir: configDir, Identity: id}, claudeSessionRuns)
			var held *claudeidentity.HeldError
			if errors.As(err, &held) {
				return "", held.ClaudeSession, nil
			}
			return replaced, "", err
		},
		holder: claudeidentity.Holder,
		other:  codexidentity.Holder, otherKind: "Codex thread",
		release: func() (string, string, bool, error) {
			rec, freed, err := claudeidentity.Release(key, configDir)
			return rec.Me, rec.Root, freed, err
		},
		doorbell: coopJoinDoorbell,
	}, nil
}

func codexJoinSession(thread string) (*joinSession, error) {
	codexHome, err := codexHomeDir()
	if err != nil {
		return nil, ActionRequiredError("cannot resolve CODEX_HOME for this Codex thread: %v", err)
	}
	codexHome = absPath(filepath.Clean(codexHome))
	return &joinSession{
		harness: harness.Codex, key: thread,
		kind: "Codex thread", place: "chat", whose: "this Codex thread's",
		envHint: "for example from the environment Codex gives its shell tool",
		// A later join tells a holder that runs from one that ended by
		// this lock, so a thread without it cannot hold a handle.
		runs: func() error {
			path := codexWriterLockPath(codexHome, thread)
			held, err := writerlock.Held(context.Background(), path)
			switch {
			case err != nil:
				return ActionRequiredError("coop join cannot tell whether Codex thread %s runs: its writer lock %s cannot be inspected (%v); a later join could not tell this thread from one that ended", thread, path, err)
			case !held:
				return ActionRequiredError("coop join found Codex thread %s not loaded: its writer lock %s is not held; run it through the shell tool of that thread, with the CODEX_HOME that Codex uses", thread, path)
			}
			return nil
		},
		held: func() (string, bool, error) {
			rec, err := codexidentity.Lookup(thread, codexHome)
			if errors.Is(err, codexidentity.ErrNone) {
				return "", false, nil
			}
			return rec.Me, err == nil, err
		},
		defaultHandle: func() (string, string) {
			return harness.Codex, " The handle is the default; --me <handle> chooses another."
		},
		bind: func(id launch.Identity) (string, string, error) {
			replaced, err := codexidentity.Bind(codexidentity.Record{
				Thread: thread, CodexHome: codexHome, Root: id.Root, BaseRoot: id.BaseRoot, Session: id.Session,
				Me: id.Me, RootID: id.RootID, BaseRootID: id.BaseRootID,
			}, codexThreadRuns)
			var held *codexidentity.HeldError
			if errors.As(err, &held) {
				return "", held.Thread, nil
			}
			return replaced, "", err
		},
		holder: codexidentity.Holder,
		other:  claudeidentity.Holder, otherKind: "Claude session",
		release: func() (string, string, bool, error) {
			rec, freed, err := codexidentity.Release(thread, codexHome)
			return rec.Me, rec.Root, freed, err
		},
		doorbell: codexJoinDoorbell,
	}, nil
}

// coopJoinBeforeBind runs, when set, between a join's checks and its bind.
var coopJoinBeforeBind func()

// withCoopJoinLock runs fn under the per-user lock every coop join takes.
func withCoopJoinLock(fn func() error) error {
	dir, err := amqhome.EnsureDir()
	if err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(filepath.Join(dir, "coop-join.lock"), fn)
}

// coopJoinStoreError makes a holder record that cannot be read a refusal:
// the session it names may still hold the handle.
func coopJoinStoreError(handle, root string, err error) error {
	var claudeErr *claudeidentity.UnreadableHolderError
	var codexErr *codexidentity.UnreadableHolderError
	if errors.As(err, &claudeErr) || errors.As(err, &codexErr) {
		return ActionRequiredError("handle %s in %s cannot be joined: %v; inspect that record, or choose another handle with --me <handle>", handle, root, err)
	}
	return err
}

// codexWriterLockPath is the lock Codex holds while thread is loaded.
func codexWriterLockPath(codexHome, thread string) string {
	return filepath.Join(codexHome, "thread-writer-locks", thread+".lock")
}

// codexThreadRuns reports whether Codex still has the thread of holder
// loaded: its writer lock under the holder's CODEX_HOME is held. Anything
// unproven counts as running: an inspection error, or a platform where the
// lock cannot be inspected.
func codexThreadRuns(holder codexidentity.Record) bool {
	held, err := writerlock.Held(context.Background(), codexWriterLockPath(holder.CodexHome, holder.Thread))
	return err != nil || held
}

// codexJoinDoorbell says what will announce new mail to a Codex thread that
// just joined, or why nothing will.
func codexJoinDoorbell() string {
	home, err := os.UserHomeDir()
	path, perr := codexCoopHooksPath()
	if err != nil || perr != nil {
		return "No wake: run amq drain --include-body between tasks."
	}
	commands, err := hookfile.MarkedHookCommandsAt(path, "Stop", coopHookMarker)
	bin, _ := os.Executable()
	switch {
	case err != nil || len(commands) == 0:
		return "No wake: the wait hook is not installed. Run amq coop install-hooks --harness codex, approve the hook in Codex, then start a new chat and join again; until then run amq drain --include-body between tasks."
	case !slices.Contains(commands, codexCoopWaitCommand(bin, codexidentity.ThreadsDir(home))):
		return "No wake: the installed wait hook is not the one this amq installs (another binary, or an older release). Run amq coop install-hooks --harness codex, approve the hook in Codex, then start a new chat; until then run amq drain --include-body between tasks."
	}
	return "A hook wakes this chat when mail arrives after a turn ends; it starts at the end of this turn, in a chat opened after the hook was approved in Codex. Do not run amq watch, amq monitor or a polling loop."
}

func runCoopJoin(args []string) error {
	fs := flag.NewFlagSet("coop join", flag.ContinueOnError)
	me := fs.String("me", "", "Agent handle for this session (default: the one it holds; else, in Claude Code, the name the user gave the session or claude, and in Codex, codex)")
	session := fs.String("session", "", "Session name (default: the project's declared default session)")
	usage := usageWithFlags(fs, "amq coop join [--me <handle>] [--session <name>]",
		"Bind this Claude Code session or Codex thread to an agent handle. Run it",
		"inside the session, through its shell tool. It is for a session that amq",
		"coop exec did not start, such as a chat of the Claude Code or Codex desktop",
		"app. It tells the harness from CLAUDE_CODE_SESSION_ID or CODEX_THREAD_ID,",
		"and refuses a shell that has both.",
		"",
		"After a join, amq commands and the co-op hooks of this session use the handle",
		"with no --me and no AM_* variable. Under an application that hosts Claude",
		"Code, such as the desktop app, the binding belongs to the chat, by the chat",
		"id in Claude Code's session record: it survives a resume, a compaction and",
		"/clear. In a session with no host it belongs to CLAUDE_CODE_SESSION_ID: it",
		"survives a resume and a compaction, and a session after /clear joins again.",
		"In Codex the binding belongs to the thread, by CODEX_THREAD_ID.",
		"A new or forked session joins.",
		"",
		"Without --me, a session that holds a handle keeps it, and a session whose",
		"binding cannot be read is refused. A Claude Code session that holds",
		"none takes the name the user gave it in Claude Code (/rename reviewer), in",
		"lower case, when that is a handle, and otherwise claude, the handle amq coop",
		"exec claude uses. A Codex thread that holds none takes codex. The output",
		"says which one it took.",
		"",
		"The root comes from the project configuration, as for other commands. The",
		"project must be set up (amq coop init, or amq setup) and the session must",
		"exist (amq session create, which also makes the queue a project file",
		"names). The mailbox of a new handle is created.",
		"",
		"The command must run in a session that runs: under the Claude Code process",
		"of the session, found by the session record Claude Code keeps while it runs,",
		"or in a Codex thread whose writer lock Codex holds. It refuses a session",
		"that has AM_* identity variables, a handle that another running session",
		"holds (run amq coop leave there), a handle bound in the other harness, and",
		"a handle with a running wake that is not this session's own. It takes the",
		"handle of a session that does not run, such as a chat with no process, and",
		"says so; that session is then refused with the reason until it joins with",
		"--me. Joining again with the same handle changes nothing; another handle",
		"replaces the first.",
		"",
		"It prints the unread count and whether a hook wakes the session for new mail",
		"(see amq coop install-hooks). A Codex thread has no wake yet.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if fs.NArg() != 0 {
		return UsageError("coop join takes no positional arguments")
	}
	named := ""
	if *me != "" {
		var err error
		if named, err = normalizeHandle(*me); err != nil {
			return UsageError("coop join --me: %v", err)
		}
	}
	s, err := sessionHere("join")
	if err != nil {
		return err
	}
	if err := s.runs(); err != nil {
		return err
	}
	// A present identity variable is an explicit context: commands and hooks
	// would use it and never read the binding.
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			return ActionRequiredError("this session has %s in its environment, so it already has an AMQ context and a binding would not be used; remove the AM_* variables (%s) and start a new session", key, s.envHint)
		}
	}
	handle, chosenBy, err := coopJoinHandle(named, s)
	if err != nil {
		return err
	}
	base, err := resolveBaseRoot()
	if err != nil {
		return err
	}
	name := *session
	if name == "" {
		if name, err = declaredCoopExecSession(); err != nil {
			return err
		}
	}
	if err := validateSessionName(name); err != nil {
		return err
	}
	base = absPath(base)
	// With a project file, amq session create makes the queue it names;
	// with none, the project is not set up and only setup helps.
	if !dirExists(base) {
		if _, err := findAndLoadAmqrc(); errors.Is(err, errAmqrcNotFound) {
			return NotFoundError("no AMQ queue at %s; set this project up with: amq coop init", base)
		}
	}
	root := filepath.Join(base, name)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return NotFoundError("session %s does not exist at %s; create it with: amq session create %s", name, root, name)
	}
	id := launch.IdentityFromEnv(buildCoopExecEnvironment(nil, root, handle, name))
	if id.RootID == "" || id.BaseRootID == "" {
		return fmt.Errorf("cannot read the identity of root %s", root)
	}
	if id, err = validateRecordedIdentity(id, s.whose); err != nil {
		return err
	}
	// A session that holds this handle here joins again with no change, even
	// while its own wake runs (a Codex thread amq coop exec started has one).
	var (
		rejoin, ownWake bool
		rootToken       string
		replaced, held  string
	)
	// Both harnesses' joins take one lock from the check of the other
	// harness's store through the bind, so two joins of one handle from
	// different harnesses cannot both pass the check. Lock order: this lock,
	// then a store's lock.
	err = withCoopJoinLock(func() error {
		holder, holds, err := s.holder(id.Root, id.RootID, handle)
		if err != nil {
			return coopJoinStoreError(handle, root, err)
		}
		rejoin = holds && holder == s.key
		if wake := inspectWakeLock(root, handle); wake.Exists && wake.Process.Running {
			if !rejoin {
				return ActionRequiredError("handle %s in %s has a running wake (pid %d), so another session owns it; choose another handle with --me <handle>", handle, root, wake.PID)
			}
			ownWake = true
		}
		// One handle carries one session: a binding in the other harness's
		// store holds it whether or not that session runs.
		if other, ok, err := s.other(id.Root, id.RootID, handle); err != nil {
			return coopJoinStoreError(handle, root, err)
		} else if ok {
			return ActionRequiredError("handle %s in %s is bound to %s %s; run amq coop leave there, or choose another handle with --me <handle>", handle, root, s.otherKind, other)
		}
		if coopJoinBeforeBind != nil {
			coopJoinBeforeBind()
		}
		if rootToken, err = provisionSelfMailboxInExistingRoot(root, handle); err != nil {
			return err
		}
		if replaced, held, err = s.bind(id); err != nil {
			return coopJoinStoreError(handle, root, err)
		}
		if held != "" {
			return ActionRequiredError("handle %s in %s is held by the running %s %s; run amq coop leave in that %s, or choose another handle with --me <handle>", handle, root, s.kind, held, s.place)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// As coop exec does, record that the harness runs under the handle, so
	// peers see it as a registered agent. A session that already held the
	// handle keeps the record it has, which coop exec may have written with
	// more detail.
	if !rejoin {
		publishAgentIdentity(root, rootToken, handle, harness.Invocation{Harness: s.harness})
	}
	if err := writeStdout("Joined as %s (session %s, root %s).%s\n", handle, name, root, chosenBy); err != nil {
		return err
	}
	// The handle moved from a session that may come back, for example a
	// chat with no process now. Say so here; that session is refused with
	// the reason at its next command.
	if replaced != "" {
		if err := writeStdout("The handle was held by %s %s, which does not run; that %s has no handle now and joins with --me <handle>.\n", s.kind, replaced, s.place); err != nil {
			return err
		}
	}
	// This session's SessionStart ran before the join and printed no
	// preamble, so the backlog is reported here.
	if unread := coopHookUnread(root, handle); unread > 0 {
		if err := writeStdout("Inbox: %d unread. Run amq drain --include-body.\n", unread); err != nil {
			return err
		}
	}
	if ownWake {
		return writeStdout("The running wake of %s announces new mail.\n", handle)
	}
	return writeStdout("%s\n", s.doorbell())
}

// coopJoinHandle picks the handle of a join and, when the caller did not
// name it, the sentence that says where it came from. named is the handle
// --me gave, normalized, and it wins. With none, a session keeps the handle
// it holds, so a repeated join changes nothing; a binding that cannot be
// read is refused, never replaced by a default. A session that holds none
// takes its harness's default.
func coopJoinHandle(named string, s *joinSession) (handle, chosenBy string, err error) {
	if named != "" {
		return named, "", nil
	}
	held, ok, err := s.held()
	switch {
	case err != nil:
		return "", "", ActionRequiredError("this session's handle cannot be read: %v; name the handle with --me <handle>", err)
	case ok:
		return held, "", nil
	}
	handle, chosenBy = s.defaultHandle()
	return handle, chosenBy, nil
}

// claudeDefaultHandle is the handle of a Claude Code session that holds
// none: the name its user gave it in Claude Code when that is a handle once
// in lower case, and otherwise the harness name, the handle amq coop exec
// claude uses. Only a name the user gave counts, and it is taken whole or
// not at all: a name Claude Code derived from the directory, and a title
// written from the first prompt, say nothing about who the agent is. The
// human's handle is never a default.
func claudeDefaultHandle(session claudeNativeRegistry) (handle, chosenBy string) {
	if session.NameSource == claudeNameSourceUser {
		if name, err := normalizeHandle(strings.ToLower(strings.TrimSpace(session.Name))); err == nil && name != reservedHumanHandle {
			return name, " The handle is this session's name; --me <handle> chooses another."
		}
	}
	return harness.Claude, " The handle is the default; --me <handle> chooses another."
}

// claudeNameSourceUser is the nameSource of a session record whose name
// the user chose, with /rename, --name or the host application.
const claudeNameSourceUser = "user"

// coopJoinDoorbell says what will announce new mail to the session that
// just joined, or why nothing will.
func coopJoinDoorbell() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "No wake: run amq drain --include-body between tasks."
	}
	commands, err := claude.MarkedHookCommands(home, "Stop", coopHookMarker)
	bin, _ := os.Executable()
	switch {
	case err != nil || !slices.ContainsFunc(commands, ownsCoopHook(true)):
		return "No wake: the wait hook is not installed. Run amq coop install-hooks, then start a new session and join again; until then run amq drain --include-body between tasks."
	case !slices.Contains(commands, coopHookCommand(bin, claudeidentity.SessionsDir(home), "wait")):
		return "No wake: the installed wait hook is not the one this amq installs (another binary, or an older release). Run amq coop install-hooks, then start a new session; until then run amq drain --include-body between tasks."
	case !coopWaitRunsInBackground():
		return "No wake: this Claude Code session does not keep the wait hook in the background. Run amq drain --include-body between tasks."
	}
	return "A hook wakes this session when mail arrives after a turn ends; it starts at the end of this turn. Do not run amq watch, amq monitor or a polling loop."
}

// claudeRecordKey is the key of the session a record describes: the chat id
// Claude Code recorded for it, else its Claude session id.
func claudeRecordKey(record claudeNativeRegistry) string {
	if claudeidentity.ValidHostSession(record.HostSessionID) {
		return record.HostSessionID
	}
	return record.SessionID
}

// claudeKeyOf returns the key of claudeSession, the session this command
// runs in: the key of its session record, or the Claude session id when
// that record cannot be found.
func claudeKeyOf(configDir, claudeSession string) string {
	if record, ok := ancestorClaudeSessionRecord(configDir, claudeSession); ok {
		return claudeRecordKey(record)
	}
	return claudeSession
}

// ancestorClaudeSessionRecord returns the session record of the Claude Code
// process this command runs under, when that process runs claudeSession: the
// record of the nearest parent process that has one naming the session.
func ancestorClaudeSessionRecord(configDir, claudeSession string) (claudeNativeRegistry, bool) {
	record, _, ok := ancestorClaudeSession(configDir, claudeSession)
	return record, ok
}

// ancestorClaudeSession is ancestorClaudeSessionRecord with the identity of
// the process that keeps the record.
func ancestorClaudeSession(configDir, claudeSession string) (claudeNativeRegistry, launch.HolderIdentity, bool) {
	for _, ancestor := range launch.Ancestors() {
		record, err := readClaudeSessionRecord(configDir, ancestor.PID)
		if err == nil && record.SessionID == claudeSession {
			return record, ancestor, true
		}
	}
	return claudeNativeRegistry{}, launch.HolderIdentity{}, false
}

// claudeSessionRuns reports whether a Claude Code process still runs the
// session of holder. Claude Code keeps one session record per process and
// removes it at exit, so the session runs when a record in its config
// directory has its key and that record's process is alive. A session that
// ended (the conversation was cleared or deleted, or Claude Code crashed)
// can no longer run amq coop leave. Anything unproven counts as running: a
// directory that cannot be listed, an unreadable record of a live process,
// and a record of the session whose pid is of another system.
func claudeSessionRuns(holder claudeidentity.Record) bool {
	entries, err := os.ReadDir(filepath.Join(holder.ConfigDir, "sessions"))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	for _, entry := range entries {
		name, isRecord := strings.CutSuffix(entry.Name(), ".json")
		pid, err := strconv.Atoi(name)
		if !isRecord || err != nil || pid <= 0 {
			continue
		}
		record, err := readClaudeSessionRecord(holder.ConfigDir, pid)
		if err != nil {
			if wakemutation.ProcessAlive(pid) {
				return true
			}
			continue
		}
		if claudeRecordKey(record) != holder.ClaudeSession {
			continue
		}
		if (record.PidDomain != "" && record.PidDomain != runtime.GOOS) || wakemutation.ProcessAlive(pid) {
			return true
		}
	}
	return false
}

func runCoopLeave(args []string) error {
	fs := flag.NewFlagSet("coop leave", flag.ContinueOnError)
	usage := usageWithFlags(fs, "amq coop leave",
		"Release the handle that amq coop join bound to this Claude Code session or",
		"Codex thread. The mailbox and its messages stay. After it, amq commands in",
		"this session resolve their context as in a session that never joined, and",
		"another session can join with the handle. When another session took the",
		"handle after this one ended and came back, that session keeps it.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	s, err := sessionHere("leave")
	if err != nil {
		return err
	}
	me, root, freed, err := s.release()
	if errors.Is(err, claudeidentity.ErrNone) || errors.Is(err, codexidentity.ErrNone) {
		return writeStdout("This session has no handle.\n")
	}
	if err != nil {
		return err
	}
	if !freed {
		return writeStdout("Left: another session had taken %s in %s and keeps it.\n", me, root)
	}
	return writeStdout("Left: %s in %s is free.\n", me, root)
}
