//go:build darwin || linux

package cli

import (
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

	"github.com/avivsinai/agent-message-queue/internal/claudeidentity"
	"github.com/avivsinai/agent-message-queue/internal/cli/wakemutation"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// amq coop join binds the Claude Code session it runs in to one handle, so
// a session amq coop exec did not start (a session of the desktop app) is
// one agent. The binding is per session: two sessions in one project join
// with two handles. Wake policy stays with the handle, in the wake settings.

// coopJoinCanRun reports whether amq coop join would get past its checks of
// the session this command runs in: a Claude Code session with a session
// record among this process's ancestors and no AM_* identity variable. A
// hint names the join only then.
func coopJoinCanRun() bool {
	session, configDir, ok := claudeSessionKey()
	if !ok {
		return false
	}
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			return false
		}
	}
	_, ok = ancestorClaudeSessionRecord(configDir, session)
	return ok
}

func runCoopJoin(args []string) error {
	fs := flag.NewFlagSet("coop join", flag.ContinueOnError)
	me := fs.String("me", "", "Agent handle for this session (default: the one it holds, else the name the user gave the session, else claude)")
	session := fs.String("session", "", "Session name (default: the project's declared default session)")
	usage := usageWithFlags(fs, "amq coop join [--me <handle>] [--session <name>]",
		"Bind this Claude Code session to an agent handle. Run it inside the session,",
		"through its shell tool. It is for a session that amq coop exec did not start,",
		"such as a session of the Claude Code desktop app.",
		"",
		"After a join, amq commands and the co-op hooks of this session use the handle",
		"with no --me and no AM_* variable: they find the binding by",
		"CLAUDE_CODE_SESSION_ID. The binding lasts as long as the session id: it",
		"survives a resume and a compaction. A new or forked session, and a session",
		"after /clear, has a new id and joins again.",
		"",
		"Without --me, a session that holds a handle keeps it, and a session whose",
		"binding cannot be read is refused. A session that holds",
		"none takes the name the user gave it in Claude Code (/rename reviewer), in",
		"lower case, when that is a handle, and otherwise claude, the handle amq coop",
		"exec claude uses. The output says which one it took.",
		"",
		"The root comes from the project configuration, as for other commands. The",
		"project must be set up (amq coop init, or amq setup) and the session must",
		"exist (amq session create, which also makes the queue a project file",
		"names). The mailbox of a new handle is created.",
		"",
		"The command must run under the Claude Code process of the session; it finds",
		"that process by the session record Claude Code keeps while it runs. It",
		"refuses a session that has AM_* identity variables, a handle that another",
		"running Claude session holds (run amq coop leave there), and a handle with a",
		"running wake. It takes the handle of a session that no longer runs, for",
		"example one that /clear ended. Joining again with the same handle changes",
		"nothing; another handle replaces the first.",
		"",
		"It prints the unread count and whether a hook wakes the session for new mail",
		"(see amq coop install-hooks).")
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
	claudeSession, configDir, ok := claudeSessionKey()
	if !ok {
		return ActionRequiredError("coop join must run inside a Claude Code session (%s is not set to a session id)", envClaudeSession)
	}
	// A later join tells a holder that runs from one that ended by this
	// record, so a session without one cannot hold a handle.
	claudeRecord, ok := ancestorClaudeSessionRecord(configDir, claudeSession)
	if !ok {
		return ActionRequiredError("coop join found no Claude Code session record for session %s (%s) among the processes this command runs under; run it through the shell tool of that session, on a current Claude Code", claudeSession, filepath.Join(configDir, "sessions"))
	}
	// A present identity variable is an explicit context: commands and hooks
	// would use it and never read the binding.
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			return ActionRequiredError("this session has %s in its environment, so it already has an AMQ context and a binding would not be used; remove the AM_* variables (for example from the Claude settings env block) and start a new session", key)
		}
	}
	handle, chosenBy, err := coopJoinHandle(named, claudeRecord, claudeSession, configDir)
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
	if wake := inspectWakeLock(root, handle); wake.Exists && wake.Process.Running {
		return ActionRequiredError("handle %s in %s has a running wake (pid %d), so another session owns it; choose another handle with --me <handle>", handle, root, wake.PID)
	}
	rootToken, err := provisionSelfMailboxInExistingRoot(root, handle)
	if err != nil {
		return err
	}
	id := launch.IdentityFromEnv(buildCoopExecEnvironment(nil, root, handle, name))
	if id.RootID == "" || id.BaseRootID == "" {
		return fmt.Errorf("cannot read the identity of root %s", root)
	}
	if id, err = validateRecordedIdentity(id, "this Claude session's"); err != nil {
		return err
	}
	err = claudeidentity.Bind(claudeidentity.Record{ClaudeSession: claudeSession, ConfigDir: configDir, Identity: id}, claudeSessionRuns)
	var held *claudeidentity.HeldError
	if errors.As(err, &held) {
		return ActionRequiredError("handle %s in %s is held by the running Claude session %s; run amq coop leave in that session, or choose another handle with --me <handle>", handle, root, held.ClaudeSession)
	}
	if err != nil {
		return err
	}
	// As coop exec does, record that Claude Code runs under the handle, so
	// peers see it as a registered agent.
	publishAgentIdentity(root, rootToken, handle, harness.Invocation{Harness: harness.Claude})
	if err := writeStdout("Joined as %s (session %s, root %s).%s\n", handle, name, root, chosenBy); err != nil {
		return err
	}
	// This session's SessionStart ran before the join and printed no
	// preamble, so the backlog is reported here.
	if unread := coopHookUnread(root, handle); unread > 0 {
		if err := writeStdout("Inbox: %d unread. Run amq drain --include-body.\n", unread); err != nil {
			return err
		}
	}
	return writeStdout("%s\n", coopJoinDoorbell())
}

// coopJoinHandle picks the handle of a join and, when the caller did not
// name it, the sentence that says where it came from. named is the handle
// --me gave, normalized, and it wins. With none, a session keeps the handle
// it holds, so a repeated join changes nothing; a binding that cannot be
// read is refused, never replaced by a default. A session that holds none
// takes the name its user gave it in Claude Code when that is a handle once
// in lower case, and otherwise the harness name, the handle amq coop exec
// claude uses. Only a name the user gave counts, and it is taken whole or
// not at all: a name Claude Code derived from the directory, and a title
// written from the first prompt, say nothing about who the agent is. The
// human's handle is never a default.
func coopJoinHandle(named string, session claudeNativeRegistry, claudeSession, configDir string) (handle, chosenBy string, err error) {
	if named != "" {
		return named, "", nil
	}
	rec, err := claudeidentity.Lookup(claudeSession, configDir)
	switch {
	case err == nil:
		return rec.Me, "", nil
	case !errors.Is(err, claudeidentity.ErrNone):
		return "", "", ActionRequiredError("this session's handle cannot be read: %v; name the handle with --me <handle>", err)
	}
	if session.NameSource == claudeNameSourceUser {
		if name, err := normalizeHandle(strings.ToLower(strings.TrimSpace(session.Name))); err == nil && name != reservedHumanHandle {
			return name, " The handle is this session's name; --me <handle> chooses another.", nil
		}
	}
	return harness.Claude, " The handle is the default; --me <handle> chooses another.", nil
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
		return "No wake: the installed wait hook runs another amq than this one. Run amq coop install-hooks; until then run amq drain --include-body between tasks."
	case !coopWaitRunsInBackground():
		return "No wake: this Claude Code session does not keep the wait hook in the background. Run amq drain --include-body between tasks."
	}
	return "A hook wakes this session when mail arrives after a turn ends; it starts at the end of this turn. Do not run amq watch, amq monitor or a polling loop."
}

// ancestorClaudeSessionRecord returns the session record of the Claude Code
// process this command runs under, when that process runs claudeSession: the
// record of the nearest parent process that has one naming the session.
func ancestorClaudeSessionRecord(configDir, claudeSession string) (claudeNativeRegistry, bool) {
	for _, ancestor := range launch.Ancestors() {
		record, err := readClaudeSessionRecord(configDir, ancestor.PID)
		if err == nil && record.SessionID == claudeSession {
			return record, true
		}
	}
	return claudeNativeRegistry{}, false
}

// claudeSessionRuns reports whether a Claude Code process still runs the
// session of holder. Claude Code keeps one session record per process and
// removes it at exit, so the session runs when a record in its config
// directory names it and that record's process is alive. A session that
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
		if record.SessionID != holder.ClaudeSession {
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
		"Release the handle that amq coop join bound to this Claude Code session.",
		"The mailbox and its messages stay. After it, amq commands in this session",
		"resolve their context as in a session that never joined, and another",
		"session can join with the handle. When another session took the handle",
		"after this one ended and came back, that session keeps it.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	claudeSession, configDir, ok := claudeSessionKey()
	if !ok {
		return ActionRequiredError("coop leave must run inside a Claude Code session (%s is not set to a session id)", envClaudeSession)
	}
	rec, freed, err := claudeidentity.Release(claudeSession, configDir)
	if errors.Is(err, claudeidentity.ErrNone) {
		return writeStdout("This session has no handle.\n")
	}
	if err != nil {
		return err
	}
	if !freed {
		return writeStdout("Left: another session had taken %s in %s and keeps it.\n", rec.Me, rec.Root)
	}
	return writeStdout("Left: %s in %s is free.\n", rec.Me, rec.Root)
}
