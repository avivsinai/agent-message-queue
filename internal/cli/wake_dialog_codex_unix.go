//go:build darwin || linux

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

// The Codex approval guard. Codex runs PermissionRequest just before it
// shows an approval prompt, and the receiver opens the dialog marker with
// the call's key, so the wake holds its doorbell while the prompt shows.
// The earliest event that proves the prompt is gone closes it: that call's
// PostToolUse (approve runs the tool), else a turn or session event (deny,
// Esc, a new prompt, a crash). A PostToolUse of another call keeps the hold.
//
// PermissionRequest input has no tool_use_id, so the key is the turn id,
// the tool name, and a hash of the input. Codex adds tool_input.description
// (the approval reason) to the PermissionRequest input of Bash and
// apply_patch and not to their PostToolUse input, so for those two tools the
// hash covers tool_input.command only; other tools hash the whole input.

// codexDialogHookEvents are the events the installer wires, closes first so
// an install that stops part way never leaves an open without its close.
var codexDialogHookEvents = []string{
	"PostToolUse",
	"Stop",
	"UserPromptSubmit",
	"Interrupt",
	"SessionStart",
	"SessionEnd",
	"PermissionRequest",
}

// codexDialogToolMatcher names the tools whose PostToolUse can close an
// approval: shell, patch and MCP calls, the tools Codex asks approval for.
const codexDialogToolMatcher = `^(Bash|apply_patch|mcp__.*)$`

// codexDialogHookCommand is the command one Codex hook runs under the user's
// $SHELL -c. Without AM_ME, the receiver can find the seat only through the
// codexidentity store, so with neither the gate starts nothing. A close
// with AM_ME also skips when the agent directory holds no marker. Exit 2
// means something to Codex on several events, and an older amq at this path
// would exit 2 on the unknown flag, so the shell stays as the parent and
// ends in || true; there is no exec.
func codexDialogHookCommand(bin, store, verb string) string {
	gate := ""
	if verb == "close" {
		gate = `[ -n "${AM_ME-}" ] && ` + wakeDialogNoMarkerTest + ` && exit 0; `
	}
	gate += `[ -n "${AM_ME-}" ] || [ -d ` + singleQuote(store) + ` ] || exit 0; `
	return wakeDialogHookMarker + gate + singleQuote(bin) +
		" --no-update-check wake dialog --harness codex " + verb + " || true"
}

// codexDialogHookBinRe reads the binary back from codexDialogHookCommand.
var codexDialogHookBinRe = regexp.MustCompile(`; '((?:[^']|'\\'')*)' --no-update-check wake dialog --harness codex (?:open|close) \|\| true$`)

// singleQuote always single-quotes s for a POSIX shell.
func singleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func codexDialogHooks(bin, store string) []claude.CommandHook {
	hooks := make([]claude.CommandHook, 0, len(codexDialogHookEvents))
	for _, event := range codexDialogHookEvents {
		verb := "close"
		if event == "PermissionRequest" {
			verb = "open"
		}
		hook := claude.CommandHook{Event: event, Marker: wakeDialogHookMarker, Command: codexDialogHookCommand(bin, store, verb), Timeout: 5}
		switch event {
		case "PostToolUse":
			hook.Matcher = codexDialogToolMatcher
		case "SessionStart":
			hook.Matcher = "startup|resume|clear"
		case "Interrupt", "SessionEnd":
			// Codex bounds Interrupt hooks to 1-3 s; SessionEnd delays exit.
			hook.Timeout = 1
		}
		hooks = append(hooks, hook)
	}
	return hooks
}

// codexDialogStore is the codexidentity directory the gate tests.
func codexDialogStore() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return codexidentity.CurrentDir(home), nil
}

// adoptCodexHookIdentity gives a Codex hook with no AM_* the identity amq
// coop exec recorded for its thread: the hook input's session_id is the
// thread id, and Codex passes no CODEX_THREAD_ID to hooks. A recorded
// identity that cannot be used is an error, so the receiver does nothing.
func adoptCodexHookIdentity(sessionID string) error {
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			return nil
		}
	}
	if _, present := os.LookupEnv("CODEX_THREAD_ID"); present || !codexidentity.ValidThread(sessionID) {
		return nil
	}
	if err := os.Setenv("CODEX_THREAD_ID", sessionID); err != nil {
		return err
	}
	return adoptCodexThreadIdentity()
}

// codexCallKey hashes the part of a tool input that a PermissionRequest and
// the PostToolUse of the same call share.
func codexCallKey(tool string, input json.RawMessage) string {
	part := input
	if tool == "Bash" || tool == "apply_patch" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(input, &fields) == nil && fields["command"] != nil {
			part = fields["command"]
		}
	}
	canon := []byte("null")
	dec := json.NewDecoder(bytes.NewReader(part))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		if raw, err := json.Marshal(v); err == nil {
			canon = raw
		}
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// writeCodexDialogMarker opens the hold for one approval prompt. Codex can
// ask for two parallel calls of one turn whose inputs hash the same (the
// same command in two working directories: the hook input carries no
// working directory), and each closes with its own PostToolUse. So a second
// open in the same turn makes the marker ambiguous: no PostToolUse removes
// it, and a turn or session event does. Opens and PostToolUse closes run
// under the marker lock, so neither loses the other's update. Without the
// lock the open writes an ambiguous marker, which only holds longer.
func writeCodexDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	marker := wakeDialogMarker{
		Schema: 1, Event: in.Event, Harness: "codex", Tool: in.Tool,
		TurnID: in.TurnID, ToolInputSHA256: codexCallKey(in.Tool, in.ToolInput),
		SessionID: in.SessionID, OpenedAt: time.Now().UTC().Format(time.RFC3339),
	}
	unlock, err := lockCodexDialogMarker(dirfd)
	if err == nil {
		defer unlock()
		if current, exists, readErr := readCodexDialogMarker(dirfd, agentDir); readErr != nil ||
			exists && (current.Harness != "codex" || current.TurnID == "" || current.TurnID == in.TurnID) {
			marker.Ambiguous = true
		}
	} else {
		marker.Ambiguous = true
	}
	if marker.Ambiguous {
		marker.Tool, marker.ToolInputSHA256 = "", ""
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return replaceWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'))
}

// clearCodexDialogMarker removes the marker. A PostToolUse removes only the
// marker of its own call, under the marker lock; an ambiguous or unreadable
// marker, or another call's, stays until a turn or session event.
func clearCodexDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	if in.Event == "PostToolUse" {
		unlock, err := lockCodexDialogMarker(dirfd)
		if err != nil {
			return err
		}
		defer unlock()
		marker, exists, err := readCodexDialogMarker(dirfd, agentDir)
		if err != nil || !exists || marker.Ambiguous || marker.Harness != "codex" || marker.TurnID == "" ||
			marker.TurnID != in.TurnID || marker.Tool != in.Tool || marker.ToolInputSHA256 != codexCallKey(in.Tool, in.ToolInput) {
			return err
		}
	}
	return clearWakeDialogMarker(dirfd, agentDir, "")
}

// readCodexDialogMarker reads the marker; a marker that does not parse is an
// error, which keeps the hold.
func readCodexDialogMarker(dirfd int, agentDir *wakeAgentDir) (wakeDialogMarker, bool, error) {
	var marker wakeDialogMarker
	raw, _, exists, err := readWakeRepairMetadataAt(
		dirfd,
		wakeDialogFileName,
		"wake dialog marker",
		filepath.Join(agentDir.path, wakeDialogFileName),
		maxWakeMetadataFileBytes,
	)
	if err != nil || !exists {
		return marker, exists, err
	}
	return marker, true, json.Unmarshal(raw, &marker)
}

// codexDialogLockName is the marker lock in the agent directory. It is
// never removed; the wake never takes it.
const codexDialogLockName = ".wake.dialog.lock"

// lockCodexDialogMarker takes the marker lock and returns its release.
func lockCodexDialogMarker(dirfd int) (func(), error) {
	fd, err := unix.Openat(dirfd, codexDialogLockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return func() { _ = unix.Close(fd) }, nil
}

// codexHooksPath is the user hooks file Codex reads: $CODEX_HOME/hooks.json.
func codexHooksPath() (string, error) {
	codexHome, err := codexHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(absPath(filepath.Clean(codexHome)), "hooks.json"), nil
}

// codexHookTrustNote is printed after every Codex install: Codex skips a new
// or changed hook until the person trusts it.
const codexHookTrustNote = "Codex runs the new hooks only after you trust them: open /hooks in Codex and trust\n" +
	"the AMQ_DIALOG_HOOK entries (or choose \"Trust all\" at the next start).\n"

// installCodexDialogHook writes the Codex entries after the person's own,
// so the trust keys of their hooks do not move.
func installCodexDialogHook(bin string, dryRun bool) error {
	store, err := codexDialogStore()
	if err != nil {
		return fmt.Errorf("resolve home for the Codex dialog hook: %w", err)
	}
	hooks := codexDialogHooks(bin, store)
	if dryRun {
		return printDialogHooks(hooks)
	}
	path, err := codexHooksPath()
	if err != nil {
		return err
	}
	if err := claude.InstallCommandHooksAt(path, hooks); err != nil {
		return err
	}
	return writeStdout("dialog hook installed in %s\n%s", path, codexHookTrustNote)
}

func uninstallCodexDialogHook() error {
	path, err := codexHooksPath()
	if err != nil {
		return err
	}
	if err := claude.UninstallCommandHooksAt(path, wakeDialogHookMarker, codexDialogHookEvents); err != nil {
		return err
	}
	return writeStdout("dialog hook removed from %s\n", path)
}

// codexRequirementsPath is the system requirements file of Codex on Unix.
var codexRequirementsPath = "/etc/codex/requirements.toml"

// checkCodexDialogHook reports, read only, why the Codex approval guard
// would not run: user hooks turned off, entries missing, a missing binary or
// a stale form (command, matcher or timeout), or entries Codex skips: not
// trusted as they are now, or disabled in /hooks. It reads the user layer and
// the system requirements only.
func checkCodexDialogHook() (doctorCheck, bool) {
	path, err := codexHooksPath()
	if err != nil {
		return doctorCheck{}, false
	}
	codexHome := filepath.Dir(path)
	if info, err := os.Stat(codexHome); err != nil || !info.IsDir() {
		return doctorCheck{}, false
	}
	check := doctorCheck{Name: "Codex dialog hook", Status: "warn"}
	var config struct {
		Features map[string]any `toml:"features"`
		Hooks    struct {
			State map[string]struct {
				TrustedHash string `toml:"trusted_hash"`
				Enabled     *bool  `toml:"enabled"`
			} `toml:"state"`
		} `toml:"hooks"`
	}
	if raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml")); err == nil {
		_ = toml.Unmarshal(raw, &config)
	}
	var reqs struct {
		AllowManagedHooksOnly bool           `toml:"allow_managed_hooks_only"`
		Features              map[string]any `toml:"features"`
	}
	if raw, err := os.ReadFile(codexRequirementsPath); err == nil {
		_ = toml.Unmarshal(raw, &reqs)
	}
	if config.Features["hooks"] == false || config.Features["codex_hooks"] == false || reqs.Features["hooks"] == false || reqs.AllowManagedHooksOnly {
		check.Message = "Codex runs no user hooks (features.hooks = false or allow_managed_hooks_only), so a wake doorbell can answer a Codex approval prompt"
		return check, true
	}
	store, err := codexDialogStore()
	if err != nil {
		return doctorCheck{}, false
	}
	entries, err := readCodexHookEntries(path)
	if err != nil {
		check.Message = fmt.Sprintf("read %s: %v", path, err)
		return check, true
	}
	remedy := "remedy: amq wake install-dialog-hook --harness codex"
	var missing []string
	for _, event := range codexDialogHookEvents {
		if len(entries[event]) == 0 {
			missing = append(missing, event)
		}
	}
	switch {
	case len(missing) == len(codexDialogHookEvents):
		check.Message = "not installed: a wake doorbell can answer a Codex approval prompt; " + remedy
		return check, true
	case len(missing) > 0:
		check.Message = fmt.Sprintf("partly installed, missing %s; %s", strings.Join(missing, ", "), remedy)
		return check, true
	}
	for _, event := range codexDialogHookEvents {
		for _, entry := range entries[event] {
			match := codexDialogHookBinRe.FindStringSubmatch(entry.Command)
			if match == nil {
				check.Message = fmt.Sprintf("stale form on %s; %s", event, remedy)
				return check, true
			}
			bin := strings.ReplaceAll(match[1], `'\''`, `'`)
			if _, err := os.Stat(bin); err != nil {
				check.Message = fmt.Sprintf("the hook runs a missing amq binary %s; %s", bin, remedy)
				return check, true
			}
			want := codexDialogHooks(bin, store)
			current := want[slices.IndexFunc(want, func(h claude.CommandHook) bool { return h.Event == event })]
			if entry.Command != current.Command || entry.Matcher != current.Matcher || entry.Timeout != current.Timeout {
				check.Message = fmt.Sprintf("stale form on %s (command, matcher or timeout); %s", event, remedy)
				return check, true
			}
			state := config.Hooks.State[entry.key(path, event)]
			switch {
			case state.Enabled != nil && !*state.Enabled:
				check.Message = fmt.Sprintf("the %s entry is disabled in Codex /hooks, so Codex skips it; enable it in /hooks", event)
				return check, true
			case state.TrustedHash != entry.hash(event):
				check.Message = "installed, not trusted as it is now, so Codex skips it; open /hooks in Codex and trust the AMQ_DIALOG_HOOK entries"
				return check, true
			}
		}
	}
	check.Status = "ok"
	check.Message = "installed"
	return check, true
}

// codexHookEntry is one marked handler of the Codex hooks file, with the
// fields Codex hashes for trust. Timeout is -1 when the file sets none.
type codexHookEntry struct {
	group, handler    int
	matcherSet        bool
	Matcher           string
	Command           string
	Timeout           int
	Async             bool
	StatusMessage     *string
	AdditionalContext *int
}

// readCodexHookEntries reads the AMQ_DIALOG_HOOK handlers of path by event.
func readCodexHookEntries(path string) (map[string][]codexHookEntry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Hooks map[string][]struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Command           string  `json:"command"`
				Timeout           *int    `json:"timeout"`
				Async             bool    `json:"async"`
				StatusMessage     *string `json:"statusMessage"`
				AdditionalContext *int    `json:"additionalContextLimit"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	out := map[string][]codexHookEntry{}
	for event, groups := range file.Hooks {
		for g, group := range groups {
			for h, hook := range group.Hooks {
				if !strings.HasPrefix(hook.Command, wakeDialogHookMarker) {
					continue
				}
				e := codexHookEntry{group: g, handler: h, Command: hook.Command, Async: hook.Async,
					StatusMessage: hook.StatusMessage, AdditionalContext: hook.AdditionalContext, Timeout: -1}
				if group.Matcher != nil {
					e.matcherSet, e.Matcher = true, *group.Matcher
				}
				if hook.Timeout != nil {
					e.Timeout = *hook.Timeout
				}
				out[event] = append(out[event], e)
			}
		}
	}
	return out, nil
}

// key is the hooks.state key Codex stores this handler's trust under.
func (e codexHookEntry) key(path, event string) string {
	return fmt.Sprintf("%s:%s:%d:%d", path, snakeEvent(event), e.group, e.handler)
}

// hash is the trust hash Codex computes for this handler (codex-rs
// hooks/src/engine/discovery.rs hook_hash, config/src/fingerprint.rs): the
// SHA-256 of the sorted, compact JSON of the event label, the matcher where
// the event honors one, and the normalized command handler.
func (e codexHookEntry) hash(event string) string {
	timeout := e.Timeout
	switch {
	case event == "SessionEnd" || event == "Interrupt":
		if timeout < 0 {
			timeout = 1
		}
		timeout = min(max(timeout, 1), 3)
	case timeout < 0:
		timeout = 600
	default:
		timeout = max(timeout, 1)
	}
	handler := map[string]any{"type": "command", "command": e.Command, "async": e.Async, "timeout": timeout}
	if e.StatusMessage != nil {
		handler["statusMessage"] = *e.StatusMessage
	}
	switch event {
	case "PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", "SubagentStart":
		if e.AdditionalContext != nil && *e.AdditionalContext != 2500 {
			handler["additionalContextLimit"] = *e.AdditionalContext
		}
	}
	identity := map[string]any{"event_name": snakeEvent(event), "hooks": []any{handler}}
	if e.matcherSet && event != "Stop" && event != "UserPromptSubmit" && event != "Interrupt" {
		identity["matcher"] = e.Matcher
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(identity)
	sum := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// snakeEvent is Codex's spelling of an event in a trust key.
func snakeEvent(event string) string {
	var b strings.Builder
	for i, r := range event {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
