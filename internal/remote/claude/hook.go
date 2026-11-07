package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/hookfile"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Stop-hook bridge: the hook is how the adapter learns a submitted turn ENDED
// — the transcript user line proves the payload was
// accepted and the assistant line proves the turn started, but only the
// Stop event carries the completion signal. The installer writes the hook
// into the TARGET's ~/.claude/settings.json; the receiver subcommand is
// fail-open by contract (exit 0 on ANY error) so a broken bridge can never
// block Claude Code — Claude Code's blocking-error code is exit 2.

// stopHookMarker is the settings.json ownership signal: every hook command
// the installer writes STARTS with this env assignment, and uninstall
// removes exactly the entries whose nested commands all carry it. It is an
// assignment, not the command word: the shell runs the quoted binary that
// follows it (an `AMQ_STOP_HOOK_BIN=<bin> claude stop-hook` form would
// execute the program `claude`).
const stopHookMarker = "AMQ_STOP_HOOK=1 "

// stopHookType is the hook event name in settings.json.
const stopHookType = "Stop"

// InstallStopHook adds the fail-open receiver to the Stop hook chain in
// the user settings.json in the Claude config directory. Idempotent: the same entry is left untouched,
// and a marked entry with another command (an older binary) is replaced.
// The settings leaf must be a
// regular file (lstat rule — a directory or symlink is refused); the write
// is fsq atomic-replace.
//
// Claude Code's durable hook scopes are the user, project, local, and
// managed settings files. None of them is a single session: session hooks
// exist only in memory inside that process
// (https://code.claude.com/docs/en/hooks). The bound session can live in
// any project, so the installer keeps this user-level hook. The receiver
// returns before any write unless that session is attached or the binding
// names it. That limits who writes, not how many bytes a session's marker
// may hold: the file is one appended line per turn, and it is removed when
// the attachment unsubscribes. User off removes only the Buzz binding.
//
// The command starts amq-remote only when a state the receiver reads
// exists: AMQ_REMOTE_BINDING, a binding file, a binding in the bindings
// directory (its tombstones are none), or the attachment sentinels.
// Everywhere else the hook costs one sh.
func InstallStopHook(home, bin string) error {
	_, err := InstallStopHookAt(home, SettingsPath(home), bin)
	return err
}

// InstallStopHookAt is InstallStopHook for the settings file settings, and
// reports whether it changed the file.
func InstallStopHookAt(home, settings, bin string) (bool, error) {
	return hookfile.Replace(settings, stopHook, stopHookEntry(home, bin))
}

// UninstallStopHook removes marked entries from the Stop chain. When no
// marked entry exists the file is untouched (no rewrite, no mtime churn).
func UninstallStopHook(home string) error {
	return hookfile.Remove(SettingsPath(home), stopHook)
}

var (
	stopHook       = hookfile.Spec{Event: stopHookType, Marker: stopHookMarker}
	permissionSpec = hookfile.Spec{Event: permissionHookType, Marker: permissionHookMarker}
)

// InstallPermissionHook adds the PermissionRequest hook that carries tool
// dialogs to Buzz: one entry with no matcher (every tool), marked
// AMQ_APPROVAL_HOOK=1, with the timeout permissionHookTimeout. bin must be
// absolute: an app-launched session's PATH may not hold it. A marked entry
// with another command is replaced; the same one is kept.
func InstallPermissionHook(home, bin string) error {
	_, err := InstallPermissionHookAt(home, SettingsPath(home), bin)
	return err
}

// InstallPermissionHookAt is InstallPermissionHook for the settings file
// settings, and reports whether it changed the file.
func InstallPermissionHookAt(home, settings, bin string) (bool, error) {
	if !filepath.IsAbs(bin) {
		return false, fmt.Errorf("the hook binary %q must be an absolute path", bin)
	}
	return hookfile.Replace(settings, permissionSpec, permissionHookEntry(home, bin))
}

// InstallCommandHooks is hookfile.InstallCommandHooksAt for the user
// settings file.
func InstallCommandHooks(home string, hooks []hookfile.CommandHook) error {
	_, err := hookfile.InstallCommandHooksAt(SettingsPath(home), hooks)
	return err
}

// UninstallCommandHooks removes the entries marked with marker from each
// event chain of the user settings file.
func UninstallCommandHooks(home, marker string, events []string) error {
	return hookfile.UninstallCommandHooksAt(SettingsPath(home), marker, events)
}

// MarkedHookCommands lists the commands marked with marker in the event
// chain of the user settings file. A missing file lists none.
func MarkedHookCommands(home, event, marker string) ([]string, error) {
	return hookfile.MarkedHookCommandsAt(SettingsPath(home), event, marker)
}

// UninstallPermissionHook removes only the marked PermissionRequest entry.
func UninstallPermissionHook(home string) error {
	return hookfile.Remove(SettingsPath(home), permissionSpec)
}

// PermissionHookState is StopHookState for the PermissionRequest entry in
// the settings file settings.
func PermissionHookState(settings string) (string, error) {
	return hookState(settings, permissionSpec)
}

// PermissionHookCommand is the command of the one marked PermissionRequest
// hook in the settings file settings, "" when it does not hold exactly one.
func PermissionHookCommand(settings string) (string, error) {
	cmds, err := hookfile.InstalledCommands(settings, permissionSpec)
	if err != nil || len(cmds) != 1 {
		return "", err
	}
	return cmds[0], nil
}

// Forms of an installed hook command (StopHookForm, PermissionHookForm).
const (
	// HookFormCurrent is what the installer writes now for that binary.
	HookFormCurrent = "current"
	// HookFormUngated starts amq-remote on every event in every session.
	HookFormUngated = "ungated"
	// HookFormStale is gated but differs from what the installer writes
	// now: its gate names paths this home no longer resolves, or its
	// timeout or form is older. The gate may skip a state the receiver
	// reads.
	HookFormStale = "stale"
)

var (
	stopHookBinRe       = regexp.MustCompile(`'((?:[^']|'\\'')*)' claude stop-hook$`)
	permissionHookBinRe = regexp.MustCompile(`'((?:[^']|'\\'')*)' claude permission-hook(?: --wait [0-9]+)?$`)
)

// StopHookForm reports the form of the one installed Stop hook in the
// settings file settings, "" when it does not hold exactly one. home roots
// the markers the current form's gate names.
func StopHookForm(home, settings string) (string, error) {
	return hookForm(settings, stopHook, func(cmd string) []byte {
		m := stopHookBinRe.FindStringSubmatch(cmd)
		if m == nil {
			return nil
		}
		return stopHookEntry(home, shellUnquote(m[1]))
	})
}

// PermissionHookForm is StopHookForm for the PermissionRequest hook.
func PermissionHookForm(home, settings string) (string, error) {
	return hookForm(settings, permissionSpec, func(cmd string) []byte {
		m := permissionHookBinRe.FindStringSubmatch(cmd)
		if m == nil {
			return nil
		}
		return permissionHookEntry(home, shellUnquote(m[1]))
	})
}

// FormFlagged reports whether doctor flags an installed hook's form, so
// doctor --fix and the machine agent upgrade it.
func FormFlagged(form string) bool {
	return form == HookFormStale || form == HookFormUngated
}

// hookForm compares the one marked group of spec's chain with the group
// render builds from its command, nil when the command does not parse.
func hookForm(path string, spec hookfile.Spec, render func(cmd string) []byte) (string, error) {
	groups, err := hookfile.MarkedGroups(path, spec)
	if err != nil || len(groups) != 1 {
		return "", err
	}
	cmds, err := hookfile.InstalledCommands(path, spec)
	if err != nil || len(cmds) != 1 {
		return "", err
	}
	switch {
	case !strings.Contains(cmds[0], " || exec "):
		return HookFormUngated, nil
	case hookfile.SameJSON(groups[0], render(cmds[0])):
		return HookFormCurrent, nil
	}
	return HookFormStale, nil
}

func shellUnquote(s string) string { return strings.ReplaceAll(s, `'\''`, `'`) }

// Hook states in a settings file (StopHookState, PermissionHookState).
const (
	HookPresent  = "installed"
	HookMissing  = "missing"
	HookDisabled = "disabled" // present, but disableAllHooks is true
)

// StopHookState reports whether the user settings file under home holds the
// AMQ Stop hook, and whether that file disables all hooks. Without an
// effective hook a Claude request is admitted but never completes. It reads
// only ~/.claude/settings.json, through the installer's bounded reader;
// project or managed settings can still change the effective result.
func StopHookState(home string) (string, error) {
	return StopHookStateAt(SettingsPath(home))
}

// StopHookStateAt is StopHookState for the settings file at path.
func StopHookStateAt(path string) (string, error) {
	return hookState(path, stopHook)
}

func hookState(path string, spec hookfile.Spec) (string, error) {
	raw, err := hookfile.ReadBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		return HookMissing, nil
	}
	if err != nil {
		return "", err
	}
	found, err := spec.Installed(raw)
	if err != nil {
		return "", err
	}
	switch {
	case !found:
		return HookMissing, nil
	case disablesAllHooks(raw):
		return HookDisabled, nil
	}
	return HookPresent, nil
}

// HooksDisabled reports whether the user settings file under home sets
// disableAllHooks, so Claude Code runs none of its hooks unless a project
// or managed setting turns them on again.
func HooksDisabled(home string) (bool, error) {
	return HooksDisabledAt(SettingsPath(home))
}

// HooksDisabledAt is HooksDisabled for the settings file at path.
func HooksDisabledAt(path string) (bool, error) {
	raw, err := hookfile.ReadBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return disablesAllHooks(raw), nil
}

func disablesAllHooks(raw []byte) bool {
	var root struct {
		DisableAllHooks bool `json:"disableAllHooks"`
	}
	_ = json.Unmarshal(raw, &root)
	return root.DisableAllHooks
}

// SettingsPath is the user settings.json Claude Code reads for the user
// whose home is home, and the file every AMQ Claude hook installer writes.
func SettingsPath(home string) string {
	return filepath.Join(harness.ClaudeConfigDir(home), "settings.json")
}

// stopHookCommand is the shell command the hook runs: the ownership marker
// assignment, then a gate of shell tests, then the single-quoted binary,
// exec'd so Claude's signals reach it. The gate repeats what the receiver
// reads before it writes (stopSessionAllowed): the binding env path, the
// binding file and every binding in the bindings directory under the AMQ
// home that home resolves now, and the attachment sentinels under $HOME,
// the home the receiver resolves. With none of them present the receiver
// writes nothing, so the hook starts nothing.
func stopHookCommand(home, bin string) string {
	remote := filepath.Join(amqhome.Under(home), "remote")
	return stopHookMarker + `[ -z "${` + binding.EnvPath + `-}" ] && [ ! -e ` + shellQuote(filepath.Join(remote, "binding.json")) +
		` ] && [ ! -e "$HOME/.claude/sessions/amq-bound" ] && ` + noLiveBindingTest(filepath.Join(remote, "bindings")) +
		` || exec ` + shellQuote(bin) + " claude stop-hook"
}

// noLiveBindingTest is a shell test that succeeds when the bindings
// directory dir holds no binding. A tombstone is no binding, so a directory
// of tombstones alone passes. A path the shell cannot list as a plain
// directory, or an entry it cannot classify, fails the test, so the
// receiver decides. Under zsh an unmatched glob is an error, so the test
// turns on nullglob there; sh and bash keep the pattern itself, which
// names no file.
func noLiveBindingTest(dir string) string {
	d := shellQuote(dir)
	return `{ [ -z "${ZSH_VERSION-}" ] || setopt nullglob; } && [ ! -L ` + d + ` ] && { [ ! -e ` + d +
		` ] || { [ -d ` + d + ` ] && [ -r ` + d + ` ] && [ -x ` + d + ` ] && f= && for f in ` + d +
		`/*.json; do case $f in *` + binding.TombstoneSuffix + `) f= ;; esac; { [ -z "$f" ] || [ ! -e "$f" ]; } && [ ! -L "$f" ] || break; f=; done && [ -z "$f" ]; }; }`
}

// shellQuote single-quotes s for POSIX sh (Claude Code runs hook commands
// through the shell).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// permissionHookCommand runs the PermissionRequest receiver by its
// absolute path, exec'd so Claude's TERM reaches it, and only
// when something can post a dialog: a machine agent's marker, or a binding
// (the binding env path, the binding file, a named binding that is not a
// tombstone) under the AMQ home that home resolves now. With none, no
// dialog can reach Buzz, so the hook starts nothing and the terminal dialog
// decides.
func permissionHookCommand(home, bin string) string {
	remote := filepath.Join(amqhome.Under(home), "remote")
	return permissionHookMarker + `[ ! -e "${` + binding.EnvPath + `-}" ] && [ ! -e ` + shellQuote(filepath.Join(remote, "binding.json")) +
		` ] && ` + noLiveBindingTest(filepath.Join(remote, "bindings")) + ` && f= && for f in ` +
		shellQuote(filepath.Join(remote, "acp", "agents", "_machine")) + `/*.json; do [ -e "$f" ] && break; f=; done && [ -z "$f" ] || exec ` +
		shellQuote(bin) + " claude permission-hook"
}

// permissionHookTimeout is the installed timeout of the PermissionRequest
// hook, in seconds, and the hook's own deadline. The hook waits until Buzz
// or the terminal answers, so the timeout only bounds a dialog nobody
// answers for a day. Claude Code documents no maximum.
const permissionHookTimeout = 86400

// permissionHookEntry is the PermissionRequest group: no matcher, and the
// timeout permissionHookTimeout.
func permissionHookEntry(home, bin string) []byte {
	return hookfile.CommandEntry(hookfile.CommandHook{Command: permissionHookCommand(home, bin), Timeout: permissionHookTimeout})
}

// stopHookEntry renders one Stop matcher group in Claude Code's settings
// schema: an OUTER object whose "hooks" array holds the command hook
// (https://code.claude.com/docs/en/hooks#configuration and
// internal/keepalive/hookinstall's SessionStart entry use the same shape).
// Stop takes no matcher. The receiver appends one line, so 5 seconds bounds
// a stuck file system without holding a stop longer.
func stopHookEntry(home, bin string) []byte {
	return hookfile.CommandEntry(hookfile.CommandHook{Command: stopHookCommand(home, bin), Timeout: 5})
}

// StopHookPayload is the subset of Claude Code's Stop-hook stdin payload
// the receiver consumes (session identity + completion). Claude Code writes
// the transcript asynchronously, so the turn's final text may reach the
// transcript only after the hook runs; LastAssistantMessage carries it.
type StopHookPayload struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	Cwd                  string `json:"cwd"`
	HookEventName        string `json:"hook_event_name"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// maxStopTextBytes bounds the final text one marker line carries. Escaped
// as JSON it stays within maxStopMarkerBytes, so a line is always read
// whole.
const maxStopTextBytes = 128 * 1024

// maxStopPayloadBytes bounds the hook's stdin. The payload carries the
// turn's whole final text, JSON-escaped; a cut payload does not decode and
// would leave the turn without its marker.
const maxStopPayloadBytes = 64 << 20

// sessionIDRe bounds the session id the receiver will turn into a file
// name: no separators, no dot segments.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// RunStopHookReceiver is the fail-open receiver body: it appends one
// turn-completion marker line {"ts":<unix ms>,"session_id":"<sid>"}, with
// the turn's final text as "text" (and "truncated" when it was cut), to
// ~/.claude/sessions/amq-stop/<sid>.jsonl for the adapter's confirmation
// poller. EVERY error exits 0 — the hook must never block Claude Code
// (exit 2 is the harness's blocking code). It must also never HANG: the
// session id is validated before it becomes a path, and the marker is
// opened no-follow and non-blocking so a symlink or FIFO planted at the
// leaf is refused rather than followed or waited on. Scope limits who may
// append. It does not cap the file. With no binding
// file the receiver follows the attachment sentinel, so user off leaves
// writes in place while that attachment still owns the session.
func RunStopHookReceiver(home string, stdin io.Reader, stdout io.Writer) int {
	defer func() { _ = recover() }() // fail-open, unconditionally
	_ = stdout
	if !noFollowSupported {
		return 0 // no safe create on this platform
	}
	var payload StopHookPayload
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStopPayloadBytes))
	if err != nil {
		return 0
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	if !sessionIDRe.MatchString(payload.SessionID) {
		return 0
	}
	// An unbound session writes nothing. The check is before the marker
	// directory is created.
	if !stopSessionAllowed(home, payload.SessionID) {
		return 0
	}
	marker := stopMarkerPath(home, payload.SessionID)
	dir := filepath.Dir(marker)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return 0 // a symlinked marker directory is never written through
	}
	pre, preErr := os.Lstat(marker)
	if preErr == nil && !pre.Mode().IsRegular() {
		return 0
	}
	f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY|openNoFollowFlag, 0o600)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	// Same-file check before writing, behind the no-follow open: the description must
	// be the regular file the path names without following a link — the
	// one lstat saw, or, when the file was just created, the one a fresh
	// lstat sees now.
	post, err := f.Stat()
	if err != nil || !post.Mode().IsRegular() {
		return 0
	}
	if preErr != nil {
		if pre, err = os.Lstat(marker); err != nil || !pre.Mode().IsRegular() {
			return 0
		}
	}
	if !os.SameFile(pre, post) {
		return 0
	}
	m := map[string]any{"ts": time.Now().UnixMilli(), "session_id": payload.SessionID}
	if payload.LastAssistantMessage != "" {
		text, cut := protocol.TruncateText(payload.LastAssistantMessage, maxStopTextBytes)
		m["text"] = text
		if cut {
			m["truncated"] = true
		}
	}
	line, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	_, _ = f.Write(append(line, '\n'))
	return 0
}

func stopMarkerPath(home, sessionID string) string {
	return filepath.Join(markerDir(home), "amq-stop", sessionID+".jsonl")
}

func stopBoundDir(home, sessionID string) string {
	return filepath.Join(markerDir(home), "amq-bound", sessionID)
}

// bindStopSession records one attachment's ownership of this session.
// Each attachment gets its own sentinel file, so one attachment's cleanup
// cannot drop a replacement that still owns the session.
func bindStopSession(home, sessionID string) (string, error) {
	if !sessionIDRe.MatchString(sessionID) {
		return "", fmt.Errorf("stop session %q is not a file-safe id", sessionID)
	}
	if !noFollowSupported {
		return "", errUnsupportedPlatform
	}
	dir := stopBoundDir(home, sessionID)
	var f *os.File
	// Another attachment's unbind removes an empty directory, so a create
	// that loses that race makes the directory again, once.
	for attempt := 0; ; attempt++ {
		err := os.MkdirAll(dir, 0o700)
		if err == nil {
			var fi os.FileInfo
			if fi, err = os.Lstat(dir); err == nil && !fi.IsDir() {
				return "", fmt.Errorf("stop bound dir %s is not a plain directory", dir)
			}
		}
		if err == nil {
			f, err = os.CreateTemp(dir, "owner-")
		}
		if err == nil {
			break
		}
		if attempt > 0 || !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	token := filepath.Base(f.Name())
	if err := f.Close(); err != nil {
		return "", err
	}
	return token, nil
}

// unbindStopSession drops one attachment's sentinel when that attachment
// unsubscribes. The marker stays while any other attachment still owns the
// session. This is not user off: off removes only the Buzz binding.
func unbindStopSession(home, sessionID, token string) {
	if !sessionIDRe.MatchString(sessionID) || !noFollowSupported || token == "" {
		return
	}
	removeRegular(filepath.Join(stopBoundDir(home, sessionID), token))
	if stopSentinelPresent(home, sessionID) {
		return
	}
	removeRegular(stopMarkerPath(home, sessionID))
	// The installed gate tests amq-bound itself, so the empty directories
	// go too: with nothing attached the Stop hook starts nothing.
	removeEmptyDir(stopBoundDir(home, sessionID))
	removeEmptyDir(filepath.Dir(stopBoundDir(home, sessionID)))
}

// removeEmptyDir removes path when it is a plain empty directory; a
// directory that holds anything stays.
func removeEmptyDir(path string) {
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		_ = os.Remove(path)
	}
}

func removeRegular(path string) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	_ = os.Remove(path)
}

// stopSessionAllowed reports whether this Stop may write. A binding file
// decides on its own: write only when it names this session. Mailbox
// bindings have no native session, so a stale sentinel must not write.
// With no binding file, an attached sentinel is enough, including after
// user off while the endpoint attachment is still subscribed.
func stopSessionAllowed(home, sessionID string) bool {
	switch bindingDecision(home, sessionID) {
	case bindingAllow:
		return true
	case bindingDeny:
		return false
	default:
		return stopSentinelPresent(home, sessionID)
	}
}

func stopSentinelPresent(home, sessionID string) bool {
	entries, err := os.ReadDir(stopBoundDir(home, sessionID))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			return true
		}
	}
	return false
}

const (
	bindingAbsent = iota
	bindingAllow
	bindingDeny
)

// bindingDecision reads every binding file: the legacy binding.json and each
// bindings/*.json beside it. Any readable file naming this session allows.
// Otherwise any binding file at all denies, readable or not. The hook
// process has only the Claude home, so an unset AMQ_REMOTE_BINDING resolves
// under that home.
func bindingDecision(home, sessionID string) int {
	path := filepath.Join(amqhome.Under(home), "remote", "binding.json")
	if p := strings.TrimSpace(os.Getenv(binding.EnvPath)); p != "" && filepath.IsAbs(p) {
		path = filepath.Clean(p)
	}
	exists := false
	if raw, err := readRegularBounded(path, bindingMaxBytes); !errors.Is(err, os.ErrNotExist) {
		exists = true
		if err == nil && nativeSessionIs(raw, sessionID) {
			return bindingAllow
		}
	}
	allow, present := bindingsDirState(filepath.Join(filepath.Dir(path), "bindings"), sessionID)
	if allow {
		return bindingAllow
	}
	if exists || present {
		return bindingDeny
	}
	return bindingAbsent
}

func nativeSessionIs(raw []byte, sessionID string) bool {
	var b struct {
		NativeSession string `json:"native_session"`
	}
	return json.Unmarshal(raw, &b) == nil && b.NativeSession == sessionID
}

// bindingMaxBytes matches the binding package's read bound.
const bindingMaxBytes = 64 << 10
