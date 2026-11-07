//go:build darwin || linux

package cli

// Purpose: guards the wake dialog guard (#1054). The regression test drives
// the real hook receiver and the real marker reader against the hold
// harness; it breaks if the wake types while a Claude Code dialog is open,
// if the hold spends the reminder budget, or if the doorbell does not ring
// in full after the dialog closes. The installer test breaks if
// install-dialog-hook misses an event, matcher or timeout, or is not
// idempotent. The gate test runs the installed command strings under /bin/sh,
// the boundary where the defect lived.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"golang.org/x/sys/unix"
)

// Regression: #1054, an injected doorbell answered an open
// AskUserQuestion dialog in Claude Code.
func TestWakeDialogHoldsDoorbellUntilDialogCloses(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	clearSendMailboxTestEnv(t)
	t.Setenv(envMe, "codex")
	t.Setenv(envRoot, h.root)
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir, "")
	hook := func(verb, input string) {
		t.Helper()
		if err := runWakeDialog([]string{verb}, strings.NewReader(input)); err != nil {
			t.Fatalf("wake dialog %s: %v", verb, err)
		}
	}

	// The transcript is missing, so it proves nothing and the hold stays.
	hook("open", `{"hook_event_name":"PreToolUse","session_id":"s1","transcript_path":"/nonexistent/amq-test/session.jsonl","tool_name":"AskUserQuestion","tool_use_id":"toolu_1","tool_input":{"questions":[]}}`)
	// The dialog opened at the harness clock's now.
	marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName, wakeDialogEntryName("s1", "", "toolu_1"))
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatal(err)
	}
	// field: held Codex doorbell invisible for 2.4 h (2026-10-25). The
	// first held attempt shows the desktop notice at once, not after
	// wakeDialogQuietHold; an unchanged count stays quiet within the
	// attention period, and a new message shows it again.
	var notices []string
	h.cfg.settings.heldNotice = true
	h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
	h.cfg.desktopNotify = func(title, body string) error {
		notices = append(notices, title+": "+body)
		return nil
	}
	// field: session3/claude doorbell held 13 h (2026-10-26). The wake log
	// named no marker event, kind or time, so the hold could not be traced;
	// the holding line now dates itself and names the marker.
	h.cfg.dialogHoldDetail = wakeDialogHoldDetailInDir(agentDir)
	h.send("a", format.PriorityNormal)
	_, stderr, _ := captureEnvOutput(t, func() error {
		h.scan(h.cfg)
		return nil
	})
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into an open dialog", h.injected)
	}
	if !regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ amq wake: holding the doorbell while a harness dialog is open \(marker event="PreToolUse" kind="" harness="claude" tool="AskUserQuestion" age=0s transcript=unreadable, holding: "open transcript: [^"]*"\)$`).MatchString(stderr) {
		t.Fatalf("holding diagnostic = %q, want a UTC-stamped line naming the marker event, kind, harness, tool, age and the unreadable transcript", stderr)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "AMQ: codex: 1 message waiting") ||
		!strings.Contains(notices[0], "Claude Code dialog open") {
		t.Fatalf("notices on the first held attempt = %q, want one for codex's 1 message", notices)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(notices) != 1 {
		t.Fatalf("notices within the attention period = %q, want still one", notices)
	}
	// post-release review v0.113.2 finding W1: each new message showed
	// another notice at once, with no cap. Only the attention ladder paces
	// the notice.
	h.send("b", format.PriorityNormal)
	h.scan(h.cfg)
	if len(notices) != 1 {
		t.Fatalf("notices after a new message within the attention period = %q, want still one", notices)
	}
	h.advance(wakeDoorbellAttentionRetryBase)
	h.scan(h.cfg)
	if len(notices) != 2 || !strings.Contains(notices[1], "2 messages waiting") {
		t.Fatalf("notices once the attention period ends = %q, want a second for 2 messages", notices)
	}
	if h.cfg.doorbell.reminderAttempts != 0 || h.cfg.doorbell.presentationConfirmed {
		t.Fatalf("hold spent the doorbell: reminderAttempts=%d presentationConfirmed=%v",
			h.cfg.doorbell.reminderAttempts, h.cfg.doorbell.presentationConfirmed)
	}
	if got, want := h.deadline(), h.now.Add(wakeDialogRecheck); !got.Equal(want) {
		t.Fatalf("recheck deadline = %v, want %v", got, want)
	}

	hook("close", `{"hook_event_name":"Stop","session_id":"s1"}`)
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after the dialog closed = %d, want 1", got)
	}
}

// How a marker holds the doorbell by its kind, age and transcript proof.
func TestWakeDialogHoldByMarkerAge(t *testing.T) {
	agyBusy := `{"schema":1,"event":"OSC7501","harness":"agy","kind":"working","opened_at":"2026-10-26T00:00:00Z"}` + "\n"
	// TRANSCRIPT is replaced by the row's transcript path.
	permission := `{"schema":1,"event":"PermissionRequest","tool":"Bash","transcript_path":"TRANSCRIPT","session_id":"s1","opened_at":"2026-11-05T19:13:00Z"}` + "\n"
	// TRANSCRIPT and AGENT_TRANSCRIPT are replaced by the row's paths.
	subagentPermission := `{"schema":1,"event":"PermissionRequest","tool":"Bash","transcript_path":"TRANSCRIPT","agent_id":"a1","session_id":"s1","opened_at":"2026-11-05T19:13:00Z"}` + "\n"
	subagentPermissionWithTranscript := `{"schema":1,"event":"PermissionRequest","tool":"Bash","transcript_path":"TRANSCRIPT","agent_id":"a1","agent_transcript_path":"AGENT_TRANSCRIPT","session_id":"s1","opened_at":"2026-11-05T19:13:00Z"}` + "\n"
	subagentQuestion := `{"schema":1,"event":"PreToolUse","tool":"AskUserQuestion","tool_use_id":"toolu_sub","transcript_path":"TRANSCRIPT","agent_id":"a1","agent_transcript_path":"AGENT_TRANSCRIPT","session_id":"s1","opened_at":"2026-11-05T19:13:00Z"}` + "\n"
	record := func(id, model string, sidechain bool, at time.Time) string {
		return fmt.Sprintf(`{"type":"assistant","isSidechain":%t,"timestamp":%q,"message":{"id":%q,"model":%q,"content":[]}}`,
			sidechain, at.UTC().Format(time.RFC3339Nano), id, model) + "\n"
	}
	assistant := func(id string, sidechain bool, at time.Time) string {
		return record(id, "claude-opus-5-5", sidechain, at)
	}
	// asked is the main-chain message whose tool call opened the dialog.
	asked := func(opened time.Time) string { return assistant("msg_asked", false, opened.Add(-time.Second)) }
	// HOST is replaced by the row's host process.
	permissionWithHost := `{"schema":1,"event":"PermissionRequest","tool":"Bash","transcript_path":"TRANSCRIPT","host":HOST,"session_id":"s1","opened_at":"2026-11-05T19:13:00Z"}` + "\n"
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	deadHost, reusedHost := self, self
	deadHost.PID = 99999999
	reusedHost.ProcessStart = "1.000000000"
	mainPermission := wakeDialogHookInput{Event: "PermissionRequest", Tool: "Bash", SessionID: "s1"}
	subagentPermissionOpen := wakeDialogHookInput{Event: "PermissionRequest", Tool: "Bash", SessionID: "s1", AgentID: "a1"}
	subagentMovedOn := func(opened time.Time) string {
		return assistant("msg_sub_asked", true, opened.Add(-time.Second)) + assistant("msg_sub_next", true, opened.Add(5*time.Minute))
	}
	movedOn := func(opened time.Time) string {
		return asked(opened) + assistant("msg_next", false, opened.Add(5*time.Minute))
	}
	for _, tt := range []struct {
		name   string
		marker string
		// age is the marker's age when the message arrives.
		age        time.Duration
		claudeHook bool
		// transcript and agentTranscript are the session and subagent
		// transcripts for a marker opened at opened; nil leaves a file
		// missing.
		transcript, agentTranscript func(opened time.Time) string
		// hooks run through the hook receiver after the marker, if any, is
		// written, and before the subagent transcript is.
		hooks []wakeDialogHookInput
		// agentPath is the subagent transcript below subagents/.
		agentPath string
		// host is the marker's host process.
		host launch.HolderIdentity
		// ancestors replaces the hook receiver's proven ancestors.
		ancestors []launch.HolderIdentity
		// noExchange makes the file system refuse an exchange rename.
		noExchange bool
		// wakeVersion is the version a live wake lock records; each open
		// must leave a marker that wake reads (no directory before 0.128.0).
		wakeVersion string
		notices     int
		rings       int
		log         string
	}{
		// field: agy busy turns showed held notices (2026-10-26). A working
		// marker younger than wakeDialogQuietHold holds without a desktop
		// notice or terminal attention.
		{name: "agy busy turn younger than the quiet hold", marker: agyBusy, age: wakeDialogQuietHold - time.Minute},
		// field agent-message-queue-zu3 (2026-11-06): a Bash permission
		// dialog stayed open for 8 hours; a doorbell typed past its 10-minute
		// marker would have approved the command. Without a new main-chain
		// assistant message, nothing is typed.
		{name: "Claude permission marker without transcript proof holds", marker: permission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, notices: 1, log: "holding input, attention only"},
		// field agent-message-queue-zu3 (2026-11-05): a permission marker
		// held a Claude Code doorbell for an hour-long turn. A new
		// main-chain assistant message proves the dialog closed.
		{name: "Claude permission marker with a newer assistant message rings", marker: permission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: movedOn, rings: 1},
		// Pre-merge review of agent-message-queue-zu3 (session 21fde719): a
		// subagent's Bash dialog stayed open 5869 s while the main chain
		// wrote 53 new assistant messages, each a false proof.
		{name: "a newer main-chain message proves no subagent dialog", marker: subagentPermission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: movedOn, notices: 1, log: "transcript=unreadable, holding"},
		{name: "a newer message in the subagent's own transcript rings", marker: subagentPermissionWithTranscript, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, rings: 1, agentTranscript: subagentMovedOn},
		{name: "a newer message in the subagent transcript Claude Code lays out rings", marker: subagentPermission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, rings: 1, agentTranscript: subagentMovedOn},
		// The main thread's Stop and permission dialogs left a background
		// subagent's open dialog unguarded.
		{name: "a main-thread Stop keeps a subagent marker", marker: subagentPermission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: movedOn, hooks: []wakeDialogHookInput{wakeDialogHookInput{Event: "Stop", SessionID: "s1"}}, notices: 1},
		{name: "a new process's SessionStart clears a subagent marker", marker: subagentPermission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, hooks: []wakeDialogHookInput{wakeDialogHookInput{Event: "SessionStart", Source: "startup", SessionID: "s1"}}, rings: 1},
		{name: "a main-thread permission keeps an unproven subagent marker", marker: subagentPermission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: func(opened time.Time) string {
				return asked(opened) + assistant("msg_next", false, opened.Add(wakeDialogQuietHold+2*time.Minute))
			}, hooks: []wakeDialogHookInput{wakeDialogHookInput{Event: "PermissionRequest", Tool: "Bash", SessionID: "s1"}}, notices: 1},
		// Pre-merge review of agent-message-queue-zu3: a synthetic API-error
		// record landed about 0.5 s before a rejected tool call's result.
		{name: "a synthetic assistant record proves nothing", marker: permission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, notices: 1, transcript: func(opened time.Time) string {
				return asked(opened) + record("msg_error", "<synthetic>", false, opened.Add(5*time.Minute))
			}},
		// Pre-merge review of agent-message-queue-zu3 (probe2): one marker
		// held either the main thread's or a subagent's dialog, so the
		// subagent's proof left the main dialog open with no trace.
		{name: "main then subagent dialog: the main dialog holds after the subagent's closes", claudeHook: true,
			hooks: []wakeDialogHookInput{mainPermission, subagentPermissionOpen}, transcript: asked, agentTranscript: subagentMovedOn, notices: 1},
		{name: "subagent then main dialog: the main dialog holds after the subagent's closes", claudeHook: true,
			hooks: []wakeDialogHookInput{subagentPermissionOpen, mainPermission}, transcript: asked, agentTranscript: subagentMovedOn, notices: 1},
		// Pre-merge review of agent-message-queue-zu3: a workflow's subagent
		// writes two directories below subagents/.
		{name: "a newer message in a workflow subagent's transcript rings", marker: subagentPermission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, agentPath: "workflows/wf_x/agent-a1.jsonl", agentTranscript: subagentMovedOn, rings: 1},
		// Pre-merge review of agent-message-queue-zu3: another terminal's
		// session events on the same handle cleared this session's marker.
		{name: "another session's SessionStart keeps this session's dialog", claudeHook: true, transcript: asked, notices: 1,
			hooks: []wakeDialogHookInput{mainPermission, {Event: "SessionStart", Source: "startup", SessionID: "s2"}}},
		{name: "a single-file marker holds after a dialog opens over it", marker: permission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, hooks: []wakeDialogHookInput{subagentPermissionOpen}, notices: 1,
			agentTranscript: func(opened time.Time) string {
				return assistant("msg_sub_asked", true, opened) + assistant("msg_sub_next", true, opened.Add(wakeDialogQuietHold+2*time.Minute))
			}},
		// Pre-merge review of agent-message-queue-zu3: where the file system
		// refused the exchange, the new dialog got no marker and was
		// unguarded once the single marker closed.
		{name: "a refused exchange replaces the single marker with the new dialog's", claudeHook: true, noExchange: true,
			marker:     `{"schema":1,"event":"PreToolUse","tool":"AskUserQuestion","tool_use_id":"toolu_q","transcript_path":"TRANSCRIPT","session_id":"s1","opened_at":"2026-11-05T19:13:00Z"}` + "\n",
			transcript: asked, notices: 1, hooks: []wakeDialogHookInput{mainPermission, {Event: "PostToolUse", Tool: "AskUserQuestion", ToolUseID: "toolu_q", SessionID: "s1"}}},
		// A Claude process that crashed left its dialog's entry with no
		// event to close it; the dialog cannot outlive its process.
		{name: "an entry whose host exited closes", marker: permissionWithHost, host: deadHost, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, rings: 1, log: "dialog host 99999999 exited; entry closed"},
		{name: "an entry whose host pid runs another process closes", marker: permissionWithHost, host: reusedHost, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, rings: 1},
		// Review of the dialog-hold release: the host was the ancestor that
		// ran an executable named claude, so an npm Claude Code, which runs
		// as node, recorded none and its entry never closed on its exit.
		{name: "an entry of an npm Claude Code closes when the process that ran its hook exits", claudeHook: true, transcript: asked,
			ancestors: []launch.HolderIdentity{self, deadHost}, rings: 1, log: "dialog host 99999999 exited; entry closed",
			hooks: []wakeDialogHookInput{{Event: "PermissionRequest", Tool: "Bash", SessionID: "s1", hostPID: deadHost.PID}}},
		// Pre-merge review of agent-message-queue-zu3: an AskUserQuestion
		// that opened past a permission marker kept that marker, so the
		// doorbell answered a dialog seconds old once that marker released.
		{name: "AskUserQuestion after a closed permission marker holds", marker: permission, age: wakeDialogQuietHold + 5*time.Minute,
			claudeHook: true, transcript: movedOn, notices: 1,
			hooks: []wakeDialogHookInput{wakeDialogHookInput{Event: "PreToolUse", Tool: "AskUserQuestion", ToolUseID: "toolu_ask", SessionID: "s1"}}},
		// Field, 2026-11-07: background subagents sat on Bash permission
		// dialogs and were stopped; their entries held the doorbell for the
		// life of the Claude Code process. SubagentStop closes them.
		{name: "a subagent's stop closes its dialog", claudeHook: true, transcript: asked, rings: 1,
			hooks: []wakeDialogHookInput{subagentPermissionOpen, {Event: "SubagentStop", SessionID: "s1", AgentID: "a1"}}},
		// Review of the wake dialog close rules: the prompt hook runs after
		// the prompt was typed, so a queued prompt closed a background
		// subagent's open dialog and the doorbell typed into it.
		{name: "a typed prompt keeps a subagent's dialog", claudeHook: true, transcript: asked, notices: 1,
			hooks: []wakeDialogHookInput{subagentPermissionOpen, {Event: "UserPromptSubmit", SessionID: "s1"}}},
		// Review of the wake dialog close rules: a rejected or Escaped
		// subagent permission dialog writes an interrupt record, not a new
		// assistant message, so its entry held until a SubagentStop that an
		// interrupt may never send.
		{name: "a rejected subagent permission rings", marker: subagentPermission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, rings: 1, agentTranscript: func(opened time.Time) string {
				return assistant("msg_sub_asked", true, opened.Add(-time.Second)) +
					fmt.Sprintf(`{"type":"user","isSidechain":true,"timestamp":%q,"message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}`,
						opened.Add(time.Minute).UTC().Format(time.RFC3339Nano)) + "\n"
			}},
		// Review of the wake dialog close rules: another call of the same
		// batch can finish while the subagent's dialog still shows.
		{name: "a subagent's other tool result keeps its dialog", marker: subagentPermission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, notices: 1, agentTranscript: func(opened time.Time) string {
				return assistant("msg_sub_asked", true, opened.Add(-time.Second)) +
					fmt.Sprintf(`{"type":"user","isSidechain":true,"timestamp":%q,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_read","content":"ok"}]}}`,
						opened.Add(time.Minute).UTC().Format(time.RFC3339Nano)) + "\n"
			}},
		// Review of the wake dialog close rules: with no clear rule, the
		// SessionEnd of a cleared session is what closes its subagent entries.
		{name: "a session's end closes its subagent's dialog", claudeHook: true, transcript: asked, rings: 1,
			hooks: []wakeDialogHookInput{subagentPermissionOpen, {Event: "SessionEnd", SessionID: "s1"}}},
		// Review of the wake dialog entries: a single-file marker closed on
		// another session's events.
		{name: "another session's Stop keeps a single-file marker", marker: permission, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, notices: 1, hooks: []wakeDialogHookInput{{Event: "Stop", SessionID: "s2"}}},
		// Review of the wake dialog entries: the proof of a subagent's
		// rejected question was searched in the main transcript only.
		{name: "a rejected question in the subagent's own transcript rings", marker: subagentQuestion, age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, transcript: asked, rings: 1, agentTranscript: func(time.Time) string {
				return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_sub","is_error":true}]}}` + "\n"
			}},
		// Pre-merge review of agent-message-queue-zu3: a marker of no known
		// writer read as stale and rang.
		{name: "unknown marker older than the quiet hold holds", marker: "{}\n", age: wakeDialogQuietHold + time.Minute,
			claudeHook: true, notices: 1},
		// Field 2026-11-07: a wake from before dialog entries read the
		// entries directory as an unreadable marker and held every doorbell
		// with no limit.
		{name: "a wake from before dialog entries gets the single-file marker and rings after the close", claudeHook: true,
			wakeVersion: "0.126.1", transcript: asked, rings: 1,
			hooks: []wakeDialogHookInput{mainPermission, {Event: "Stop", SessionID: "s1"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newWakeHoldHarness(t, wakeHoldPolicy{})
			if tt.noExchange {
				exchange := wakeDialogExchangeAt
				wakeDialogExchangeAt = func(int, string, int, string) error { return unix.EINVAL }
				t.Cleanup(func() { wakeDialogExchangeAt = exchange })
			}
			if tt.ancestors != nil {
				ancestors := wakeDialogAncestors
				wakeDialogAncestors = func() []launch.HolderIdentity { return tt.ancestors }
				t.Cleanup(func() { wakeDialogAncestors = ancestors })
			}
			for _, in := range tt.hooks {
				if wakeDialogHookVerb(in.Event) == "open" {
					// The hook writer ages markers by the real clock.
					h.now = time.Now()
				}
			}
			if tt.claudeHook {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("CLAUDE_CONFIG_DIR", "")
				bin := filepath.Join(t.TempDir(), "amq")
				if err := os.WriteFile(bin, nil, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := claude.InstallCommandHooks(home, wakeDialogHooks(bin, wakeDialogHookEventsFor(claudeVersion{}, false))); err != nil {
					t.Fatal(err)
				}
			}
			agentDir, err := openWakeAgentDir(h.root, "codex")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = agentDir.Close() })
			h.cfg.dialogSource = wakeDialogSourceInDir(agentDir, "")
			opened := h.now.Add(-tt.age)
			paths := t.TempDir()
			// The session s1 and its subagent a1, as Claude Code lays them out.
			transcript := filepath.Join(paths, "s1.jsonl")
			agentPath := tt.agentPath
			if agentPath == "" {
				agentPath = "agent-a1.jsonl"
			}
			agentTranscript := filepath.Join(paths, "s1", "subagents", agentPath)
			writeTranscript := func(path string, content func(time.Time) string) {
				t.Helper()
				if content == nil {
					return
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content(opened)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeTranscript(transcript, tt.transcript)
			if tt.marker != "" {
				host, err := json.Marshal(tt.host)
				if err != nil {
					t.Fatal(err)
				}
				raw := strings.NewReplacer("AGENT_TRANSCRIPT", agentTranscript, "TRANSCRIPT", transcript, "HOST", string(host)).Replace(tt.marker)
				marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
				if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(marker, opened, opened); err != nil {
					t.Fatal(err)
				}
			}
			if tt.wakeVersion != "" {
				writeWakeLockForTest(t, h.root, "codex", wakeLock{PID: 4242, TTY: "/dev/ttys005", Started: "2026-11-07T00:00:00Z",
					ProcessStart: "12345", BootID: "11111111-1111-1111-1111-111111111111", Executable: "amq",
					WakeMode: wakeInjectModeRaw, Generation: "live-raw-generation", ImageVersion: tt.wakeVersion})
				stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
					return wakeProcessInfo{PID: pid, Running: true, StartToken: "12345",
						BootID: "11111111-1111-1111-1111-111111111111", Executable: "amq"}
				})
			}
			for _, in := range tt.hooks {
				if err := agentDir.withFD(func(dirfd int) error {
					if wakeDialogHookVerb(in.Event) == "open" {
						in.TranscriptPath = transcript
						return writeWakeDialogMarker(dirfd, agentDir, in)
					}
					return clearWakeDialogMarker(dirfd, agentDir, in)
				}); err != nil {
					t.Fatal(err)
				}
				if info, err := os.Lstat(filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)); tt.wakeVersion != "" && err == nil && !info.Mode().IsRegular() {
					t.Fatalf("after %s the marker is %v, which a %s wake cannot read", in.Event, info.Mode(), tt.wakeVersion)
				}
			}
			writeTranscript(agentTranscript, tt.agentTranscript)
			var notices []string
			attention := 0
			h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
			h.cfg.dialogHoldDetail = wakeDialogHoldDetailInDir(agentDir)
			h.cfg.settings.heldNotice = true
			h.cfg.desktopNotify = func(title, body string) error {
				notices = append(notices, body)
				return nil
			}
			h.cfg.attentionWrite = func(p []byte) (int, error) {
				attention++
				return len(p), nil
			}
			h.send("a", format.PriorityNormal)
			_, stderr, _ := captureEnvOutput(t, func() error {
				h.scan(h.cfg)
				return nil
			})
			if got := h.rings(); got != tt.rings {
				t.Fatalf("doorbells = %d (injected %q), want %d; wake log %q", got, h.injected, tt.rings, stderr)
			}
			if len(notices) != tt.notices || (tt.rings == 0 && (attention > 0) != (tt.notices > 0)) {
				t.Fatalf("notices = %q, attention writes = %d, want %d notices", notices, attention, tt.notices)
			}
			if tt.log != "" && !strings.Contains(stderr, tt.log) {
				t.Fatalf("wake log = %q, want %q", stderr, tt.log)
			}
		})
	}
}

// Pre-merge review of fix/wake-holds-0113 (Pro finding 8): a permission
// prompt after a quiet busy turn inherited the earlier hold's notice
// ladder, so its first notice came up to 15 minutes late. A busy to dialog
// change starts a new hold with an immediate notice.
func TestWakeDialogPromptAfterABusyTurnNoticesAtOnce(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
	write := func(kind string) {
		t.Helper()
		raw := `{"schema":1,"event":"OSC7501","harness":"agy","kind":"` + kind + `","opened_at":"2026-10-26T00:00:00Z"}` + "\n"
		if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(marker, h.now, h.now); err != nil {
			t.Fatal(err)
		}
	}
	var notices []string
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir, "")
	h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
	h.cfg.settings.heldNotice = true
	h.cfg.desktopNotify = func(_, body string) error {
		notices = append(notices, body)
		return nil
	}
	h.cfg.attentionWrite = func(p []byte) (int, error) { return len(p), nil }
	write("permission")
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	write("working")
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	write("permission")
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(notices) != 2 || strings.Contains(notices[1], "busy") {
		t.Fatalf("notices = %q, want one per permission prompt", notices)
	}
}

// W1 review (Pro finding 7): a permission prompt that followed a busy turn
// the wake never sampled kept the previous prompt's notice ladder. A new
// dialog marker starts a fresh ladder; an unchanged one does not.
func TestWakeDialogNewPromptMarkerNoticesAtOnce(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
	write := func(opened string) {
		t.Helper()
		raw := `{"schema":1,"event":"OSC7501","harness":"agy","kind":"permission","opened_at":"` + opened + `"}` + "\n"
		if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(marker, h.now, h.now); err != nil {
			t.Fatal(err)
		}
	}
	var notices []string
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir, "")
	h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
	h.cfg.settings.heldNotice = true
	h.cfg.desktopNotify = func(_, body string) error {
		notices = append(notices, body)
		return nil
	}
	h.cfg.attentionWrite = func(p []byte) (int, error) { return len(p), nil }
	write("2026-10-26T00:00:00.1Z")
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(notices) != 1 {
		t.Fatalf("notices for an unchanged prompt = %q, want one", notices)
	}
	write("2026-10-26T00:00:09.2Z")
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(notices) != 2 {
		t.Fatalf("notices after a new prompt = %q, want two", notices)
	}
}

// Pre-merge review of fix/wake-holds-0113 (lead's W1 brief): the Cursor
// /rename namer typed its command and Enter after a fixed delay, while a
// startup or dialog marker could hold the doorbell. It now waits for the
// marker as the doorbell does.
func TestCoopNamedInjectWaitsForTheDialogMarker(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "cur"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fsq.AgentBase(root, "cur"), wakeDialogFileName)
	if err := os.WriteFile(marker, []byte(`{"schema":1,"event":"Startup","harness":"cursor"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if waitCoopNamedInputFree(root, "cur", "cursor", 50*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("the namer may type while the Startup marker holds")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if !waitCoopNamedInputFree(root, "cur", "cursor", 50*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("the namer still waits after the marker is gone")
	}
}

// field: session3/claude doorbell held 13 h (2026-10-26). An
// AskUserQuestion marker its own PreToolUse wrote closes at that call's
// PostToolUse, or when Claude Code writes a tool_result for its
// tool_use_id to the transcript (Esc runs no hook). Pre-merge review of
// fix/wake-holds-0113 (Pro findings 1 and 2): an equal call's PostToolUse,
// or the newest equal tool_use in the transcript, released a permission
// dialog that still showed. A permission marker has no tool_use_id, so
// only a turn or session event closes it.
func TestWakeDialogClaudeToolCallClosesItsMarker(t *testing.T) {
	const (
		permission = `{"hook_event_name":"PermissionRequest","session_id":"s1","transcript_path":"TRANSCRIPT","tool_name":"Bash","tool_input":{"command":"date > a.txt","description":"d"}}`
		askPre     = `{"hook_event_name":"PreToolUse","session_id":"s1","transcript_path":"TRANSCRIPT","tool_name":"AskUserQuestion","tool_use_id":"toolu_q","tool_input":{"questions":[{"question":"Color?"}]}}`
		askPerm    = `{"hook_event_name":"PermissionRequest","session_id":"s1","transcript_path":"TRANSCRIPT","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Color?"}]}}`
		askPost    = `{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"AskUserQuestion","tool_use_id":"toolu_q","tool_input":{"questions":[{"question":"Color?"}],"answers":{"Color?":"Red"}}}`
		equalPost  = `{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Bash","tool_use_id":"toolu_2","tool_input":{"command":"date > a.txt","description":"d"},"tool_response":{}}`
		askEsc     = `{"type":"user","toolUseResult":"User rejected tool use","message":{"content":[{"type":"tool_result","is_error":true,"tool_use_id":"toolu_q"}]}}`
		equalUse   = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"date > a.txt","description":"d"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"tool_use_id":"toolu_2"}]}}
{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}`
	)
	type step struct{ verb, input string }
	for _, tc := range []struct {
		name  string
		steps []step
		// transcript is the session transcript after the steps.
		transcript string
		closed     bool
	}{
		{"answered AskUserQuestion", []step{{"open", askPre}, {"close", askPost}}, "", true},
		{"Esc on AskUserQuestion", []step{{"open", askPre}}, askEsc, true},
		// W1 review (Pro finding 3, Codex reproducer): a PermissionRequest
		// took the tool_use_id of the marker it replaced by tool name
		// alone, so another call's PostToolUse released its dialog.
		{"another call's PostToolUse after a PermissionRequest", []step{{"open", askPre}, {"open", askPerm}, {"close", askPost}}, "", false},
		// W1 review (Pro finding 4): a tool call's open replaced a present
		// permission marker after an unlocked check, so that call's
		// PostToolUse released the permission dialog.
		{"a tool call's open after a permission dialog", []step{{"open", permission}, {"open", askPre}, {"close", askPost}}, "", false},
		{"an equal call's PostToolUse after a permission dialog", []step{{"open", permission}, {"close", equalPost}}, "", false},
		{"an equal call's result in the transcript after a permission dialog", []step{{"open", permission}}, equalUse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
				t.Fatal(err)
			}
			clearSendMailboxTestEnv(t)
			t.Setenv(envMe, "claude")
			t.Setenv(envRoot, root)
			transcript := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(transcript, []byte(tc.transcript+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, step := range tc.steps {
				input := strings.ReplaceAll(step.input, "TRANSCRIPT", transcript)
				if err := runWakeDialog([]string{step.verb}, strings.NewReader(input)); err != nil {
					t.Fatalf("wake dialog %s: %v", step.verb, err)
				}
			}
			agentDir, err := openWakeAgentDir(root, "claude")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = agentDir.Close() })
			state, err := wakeDialogSourceInDir(agentDir, "")(time.Now())
			if got := err == nil && state == wakeDialogClosed; got != tc.closed {
				t.Fatalf("marker closed = %v (%v), want %v", got, err, tc.closed)
			}
		})
	}
}

// fakeClaudeOnPath puts a claude on PATH that prints version for
// --version, the boundary install-dialog-hook and doctor read.
func fakeClaudeOnPath(t *testing.T, version string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + version + " (Claude Code)'\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestWakeInstallDialogHookWritesEveryEventOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeClaudeOnPath(t, "2.1.310")
	install := func() []byte {
		t.Helper()
		if _, err := captureStdout(t, func() error {
			return runWake([]string{"install-dialog-hook"})
		}); err != nil {
			t.Fatalf("install-dialog-hook: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	first := install()

	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(first, &settings); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		event, matcher, verb string
		timeout              int
	}{
		{"PreToolUse", "AskUserQuestion|ExitPlanMode", `--host-pid "$PPID" open`, 5},
		{"PermissionRequest", "", `--host-pid "$PPID" open`, 5},
		{"PostToolUse", "AskUserQuestion|ExitPlanMode", "close", 5},
		{"PostToolUseFailure", "AskUserQuestion|ExitPlanMode", "close", 5},
		{"UserPromptSubmit", "", "close", 5},
		{"Stop", "", "close", 5},
		{"SessionStart", "", "close", 5},
		{"SessionEnd", "", "close", 1},
	} {
		groups := settings.Hooks[want.event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s groups = %+v, want one group with one hook", want.event, groups)
		}
		command := groups[0].Hooks[0].Command
		if groups[0].Matcher != want.matcher ||
			groups[0].Hooks[0].Timeout != want.timeout ||
			!strings.HasPrefix(command, wakeDialogHookMarker) ||
			!strings.HasSuffix(command, " wake dialog "+want.verb+" || true") {
			t.Fatalf("%s entry = matcher %q timeout %d command %q, want matcher %q, timeout %d and wake dialog %s",
				want.event, groups[0].Matcher, groups[0].Hooks[0].Timeout, command, want.matcher, want.timeout, want.verb)
		}
	}

	if second := install(); string(second) != string(first) {
		t.Fatalf("second install changed settings:\n%s\nwant:\n%s", second, first)
	}

	// field: session3/claude doorbell held 13 h (2026-10-26) follow-up.
	// Before Claude Code 2.1.101 an unknown hook event made it ignore the
	// whole settings file, so a Claude Code older than an event's first
	// version gets no entry for it.
	home = t.TempDir()
	t.Setenv("HOME", home)
	fakeClaudeOnPath(t, "2.1.118")
	settings.Hooks = nil
	if err := json.Unmarshal(install(), &settings); err != nil {
		t.Fatal(err)
	}
	if groups, ok := settings.Hooks["PostToolUseFailure"]; ok {
		t.Errorf("Claude Code 2.1.118 got a PostToolUseFailure entry: %+v", groups)
	}
	if len(settings.Hooks["PostToolUse"]) != 1 {
		t.Errorf("Claude Code 2.1.118 PostToolUse = %+v, want one entry", settings.Hooks["PostToolUse"])
	}
}

// field: plain-doorbell live check 2026-10-29 (bead agent-message-queue-r6h).
// Claude Code reads $CLAUDE_CONFIG_DIR/settings.json when the variable is
// set, but the installer wrote and the dialog guard read
// ~/.claude/settings.json, so such a seat got the guard of hooks it never ran.
func TestWakeDialogHookFollowsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	fakeClaudeOnPath(t, "2.1.310")
	if dialogGuardEnabled(harness.Claude) {
		t.Fatal("dialog guard is on before any hook is installed")
	}
	if _, err := captureStdout(t, func() error {
		return runWake([]string{"install-dialog-hook"})
	}); err != nil {
		t.Fatalf("install-dialog-hook: %v", err)
	}
	if _, err := os.Stat(filepath.Join(config, "settings.json")); err != nil {
		t.Fatalf("no settings.json in CLAUDE_CONFIG_DIR: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("install wrote ~/.claude (stat err %v), want only CLAUDE_CONFIG_DIR", err)
	}
	if !dialogGuardEnabled(harness.Claude) {
		t.Fatal("dialog guard is off with the hook installed in CLAUDE_CONFIG_DIR")
	}
}

// field: same live check. With disableAllHooks in the user settings Claude
// Code runs no hook, yet the guard read the installed entries as on and a
// startup hold had no hook to release it.
func TestWakeDialogGuardIsOffWhenClaudeDisablesAllHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeClaudeOnPath(t, "2.1.310")
	if _, err := captureStdout(t, func() error {
		return runWake([]string{"install-dialog-hook"})
	}); err != nil {
		t.Fatalf("install-dialog-hook: %v", err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	settings["disableAllHooks"] = true
	if raw, err = json.Marshal(settings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if dialogGuardEnabled(harness.Claude) {
		t.Fatal("dialog guard is on although Claude Code runs no hook")
	}
}

// Regression: hook audit inventory #7, every Claude Code session started
// amq through the dialog guard on each prompt, stop, and session start and
// end, with or without AM_ME.
func TestWakeDialogHookStartsAmqOnlyForAParticipantWithWork(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "amq")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch \"$RAN\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	agent := fsq.AgentBase(root, "codex")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(agent, wakeDialogFileName)
	// Regression (#1165 review): the receiver trims AM_ROOT and cleans it
	// lexically, so a padded root or one with ".." through a symlink names
	// the marker's directory for amq but an empty directory for sh.
	if err := os.MkdirAll(fsq.AgentBase(root+" ", "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fsq.AgentBase(filepath.Join(dir, "a", "root"), "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a", "b"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	dotted := filepath.Join(dir, "link") + "/../root"

	for _, tc := range []struct {
		name       string
		env        []string
		marker     bool
		open, shut bool // whether the open and close commands start amq
	}{
		{"no AM_ME", nil, true, false, false},
		{"participant without marker", []string{"AM_ME=codex", "AM_ROOT=" + root}, false, true, false},
		// Native startup needs its positive hook even if another handler
		// removed the marker before this handler runs.
		{"native launch without marker", []string{"AM_ME=codex", "AM_ROOT=" + root, envClaudeNativeLaunch + "=bound"}, false, true, true},
		{"participant with marker", []string{"AM_ME=codex", "AM_ROOT=" + root}, true, true, true},
		{"participant without AM_ROOT", []string{"AM_ME=codex"}, false, true, true},
		{"padded AM_ROOT, marker in the trimmed root", []string{"AM_ME=codex", "AM_ROOT=" + root + " "}, true, true, true},
		{"AM_ROOT with .. through a symlink", []string{"AM_ME=codex", "AM_ROOT=" + dotted}, true, true, true},
	} {
		for _, hook := range wakeDialogHooks(fake, wakeDialogHookEvents) {
			_ = os.Remove(ran)
			_ = os.Remove(marker)
			if tc.marker {
				if err := os.WriteFile(marker, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("/bin/sh", "-c", hook.Command)
			cmd.Dir = dir
			cmd.Env = append([]string{"PATH=/usr/bin:/bin", "RAN=" + ran}, tc.env...)
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + hook.Event + `"}`)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s %s: %v: %s", tc.name, hook.Event, err, out)
			}
			_, err := os.Stat(ran)
			started := err == nil
			want := tc.shut
			if wakeDialogHookVerb(hook.Event) != "close" {
				want = tc.open
			}
			if started != want {
				t.Errorf("%s %s: started amq = %v, want %v", tc.name, hook.Event, started, want)
			}
		}
	}
}

// Regression: hook audit, the doctor regex read the binary only right after
// the marker, so a new-form hook with a missing binary passed as installed,
// and an old form passed without a remedy.
func TestDoctorReportsStaleOrBrokenDialogHook(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	oldForm := func(bin string) []claude.CommandHook {
		hooks := wakeDialogHooks(bin, wakeDialogHookEvents)
		for i := range hooks {
			hooks[i].Command = wakeDialogHookMarker + "'" + bin + "' --no-update-check wake dialog " +
				wakeDialogHookVerb(hooks[i].Event) + " || true"
		}
		return hooks
	}
	// previousForm opens without naming the process that ran the hook.
	previousForm := func(bin string) []claude.CommandHook {
		hooks := wakeDialogHooks(bin, wakeDialogHookEvents)
		for i := range hooks {
			hooks[i].Command = wakeDialogHookCommand(bin, "", wakeDialogHookVerb(hooks[i].Event))
		}
		return hooks
	}
	for _, tc := range []struct {
		name  string
		hooks []claude.CommandHook
		want  string
	}{
		{"old form", oldForm(bin), "stale form"},
		{"form without the host pid", previousForm(bin), "stale form on PreToolUse"},
		{"missing binary", wakeDialogHooks(filepath.Join(t.TempDir(), "gone", "amq"), wakeDialogHookEvents), "missing amq binary"},
		{"current", wakeDialogHooks(bin, wakeDialogHookEvents), "installed"},
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		fakeClaudeOnPath(t, "2.1.310")
		if err := claude.InstallCommandHooks(home, tc.hooks); err != nil {
			t.Fatal(err)
		}
		check, ok := checkClaudeDialogHook()
		if !ok || !strings.Contains(check.Message, tc.want) {
			t.Errorf("%s: doctor = %+v, want message with %q", tc.name, check, tc.want)
		}
	}
}
