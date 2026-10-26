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
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/pelletier/go-toml/v2"
)

// The Codex approval guard. Codex runs PermissionRequest just before it
// shows an approval prompt, and the receiver opens the dialog marker, so the
// wake holds its doorbell while the prompt shows. Only a turn or session
// event closes it: Stop, Interrupt, UserPromptSubmit, SessionStart or
// SessionEnd. A PostToolUse cannot close it: Codex gives a prompt no call
// token, and a parallel call that Codex allowed without asking emits no
// PermissionRequest while its PostToolUse can carry the same tool and input
// as the call still waiting. A doorbell due mid-turn after an approval
// waits for the turn's end.

// codexDialogHookEvents are the events the installer wires, closes first so
// an install that stops part way never leaves an open without its close.
var codexDialogHookEvents = []string{
	"Stop",
	"UserPromptSubmit",
	"Interrupt",
	"SessionStart",
	"SessionEnd",
	"PermissionRequest",
}

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

// writeTurnDialogMarker opens the hold of a Codex or Cursor guard, whose
// marker only a turn or session event removes. It records no tool_use_id,
// so no tool call's close can remove it.
func writeTurnDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput, harness string) error {
	turnID := in.TurnID
	if harness == "cursor" {
		turnID = in.GenerationID
	}
	raw, err := json.Marshal(wakeDialogMarker{
		Schema: 1, Event: in.Event, Harness: harness, Tool: in.Tool, TurnID: turnID,
		SessionID: in.SessionID, OpenedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return replaceWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'))
}

// clearTurnDialogMarker removes the marker at a turn or session event. A
// tool call's PostToolUse (Codex) or postToolUse (Cursor) keeps it: neither
// harness ties a prompt to the call that completed. A Codex Startup marker
// closes only at UserPromptSubmit: a Stop, Interrupt or session event can
// come from a daemon turn admitted before this terminal started, which
// shows nothing about its startup screens.
func clearTurnDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	if in.Event == "PostToolUse" || in.Event == "postToolUse" {
		return nil
	}
	in.ToolUseID = ""
	return removeWakeDialogMarkerIf(dirfd, agentDir, func(raw []byte) bool {
		var marker wakeDialogMarker
		if json.Unmarshal(raw, &marker) != nil {
			return true
		}
		switch {
		case marker.Event == wakeDialogStartupEvent && marker.Harness == launch.CodexProvider:
			return in.Event == "UserPromptSubmit"
		case marker.Harness == "cursor":
			return cursorDialogCloseMatches(marker, in)
		}
		return true
	})
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

// codexDialogHookUntrusted is the check message for installed AMQ entries
// that Codex skips until the person trusts them in /hooks.
const codexDialogHookUntrusted = "installed, not trusted as it is now, so Codex skips it; open /hooks in Codex and trust the AMQ_DIALOG_HOOK entries"

// codexRequirementsPath is the system requirements file of Codex on Unix.
var codexRequirementsPath = "/etc/codex/requirements.toml"

// checkCodexDialogHook reports, read only, why the Codex approval guard
// would not run: user hooks turned off, entries missing, a missing binary or
// a stale form (command, matcher, timeout, or async), or entries Codex skips: not
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
			// An async open would let the prompt show before the marker.
			if entry.Command != current.Command || entry.Matcher != current.Matcher || entry.Timeout != current.Timeout || entry.Async {
				check.Message = fmt.Sprintf("stale form on %s (command, matcher, timeout or async); %s", event, remedy)
				return check, true
			}
			state := config.Hooks.State[entry.key(path, event)]
			switch {
			case state.Enabled != nil && !*state.Enabled:
				check.Message = fmt.Sprintf("the %s entry is disabled in Codex /hooks, so Codex skips it; enable it in /hooks", event)
				return check, true
			case state.TrustedHash != entry.hash(event):
				check.Message = codexDialogHookUntrusted
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
