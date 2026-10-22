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
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/pelletier/go-toml/v2"
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

// writeCodexDialogMarker opens the hold for one approval prompt. A second
// prompt replaces the key, so the first call's PostToolUse no longer
// matches.
func writeCodexDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	raw, err := json.Marshal(wakeDialogMarker{
		Schema: 1, Event: in.Event, Harness: "codex", Tool: in.Tool,
		TurnID: in.TurnID, ToolInputSHA256: codexCallKey(in.Tool, in.ToolInput),
		SessionID: in.SessionID, OpenedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return replaceWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'))
}

// clearCodexDialogMarker removes the marker. A PostToolUse removes only the
// marker of its own call; an unreadable marker or another key stays until a
// turn or session event.
func clearCodexDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	if in.Event == "PostToolUse" {
		raw, _, exists, err := readWakeRepairMetadataAt(
			dirfd,
			wakeDialogFileName,
			"wake dialog marker",
			filepath.Join(agentDir.path, wakeDialogFileName),
			maxWakeMetadataFileBytes,
		)
		if err != nil || !exists {
			return err
		}
		var marker wakeDialogMarker
		if json.Unmarshal(raw, &marker) != nil || marker.Harness != "codex" || marker.TurnID == "" ||
			marker.TurnID != in.TurnID || marker.Tool != in.Tool || marker.ToolInputSHA256 != codexCallKey(in.Tool, in.ToolInput) {
			return nil
		}
	}
	return clearWakeDialogMarker(dirfd, agentDir, "")
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
// stale form, or entries Codex does not trust yet. It reads the user layer
// and the system requirements only.
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
	var missing []string
	installed := 0
	for _, event := range codexDialogHookEvents {
		found, err := claude.MarkedHookCommandsAt(path, event, wakeDialogHookMarker)
		if err != nil {
			check.Message = fmt.Sprintf("read %s: %v", path, err)
			return check, true
		}
		if len(found) == 0 {
			missing = append(missing, event)
		}
		for _, command := range found {
			installed++
			match := codexDialogHookBinRe.FindStringSubmatch(command)
			if match == nil {
				check.Message = fmt.Sprintf("stale form on %s; remedy: amq wake install-dialog-hook --harness codex", event)
				return check, true
			}
			bin := strings.ReplaceAll(match[1], `'\''`, `'`)
			if _, err := os.Stat(bin); err != nil {
				check.Message = fmt.Sprintf("the hook runs a missing amq binary %s; remedy: amq wake install-dialog-hook --harness codex", bin)
				return check, true
			}
			verb := "close"
			if event == "PermissionRequest" {
				verb = "open"
			}
			if command != codexDialogHookCommand(bin, store, verb) {
				check.Message = fmt.Sprintf("stale form on %s; remedy: amq wake install-dialog-hook --harness codex", event)
				return check, true
			}
		}
	}
	switch {
	case installed == 0:
		check.Message = "not installed: a wake doorbell can answer a Codex approval prompt; remedy: amq wake install-dialog-hook --harness codex"
		return check, true
	case len(missing) > 0:
		check.Message = fmt.Sprintf("partly installed, missing %s; remedy: amq wake install-dialog-hook --harness codex", strings.Join(missing, ", "))
		return check, true
	}
	keys, err := codexDialogTrustKeys(path)
	if err != nil {
		check.Message = fmt.Sprintf("read %s: %v", path, err)
		return check, true
	}
	for _, key := range keys {
		if config.Hooks.State[key].TrustedHash == "" {
			check.Message = "installed, not trusted, so Codex skips it; open /hooks in Codex and trust the AMQ_DIALOG_HOOK entries"
			return check, true
		}
	}
	check.Status = "ok"
	check.Message = "installed"
	return check, true
}

// codexDialogTrustKeys are the hooks.state keys Codex stores trust under for
// the marked entries of path: <file>:<snake_event>:<group>:<handler>.
func codexDialogTrustKeys(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	var keys []string
	for _, event := range codexDialogHookEvents {
		for g, group := range file.Hooks[event] {
			for h, hook := range group.Hooks {
				if strings.HasPrefix(hook.Command, wakeDialogHookMarker) {
					keys = append(keys, fmt.Sprintf("%s:%s:%d:%d", path, snakeEvent(event), g, h))
				}
			}
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no AMQ_DIALOG_HOOK entries")
	}
	return keys, nil
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
