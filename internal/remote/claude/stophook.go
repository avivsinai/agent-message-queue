package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// stopHookCommand is the command line the installer writes into the
// user-level Stop hook. It is one amq-remote invocation; the endpoint and
// the hook share the AMQ root through the environment the operator already
// configures (AM_ROOT / AM_ME), so no secrets are embedded in settings.
const stopHookCommand = "amq-remote claude stop-hook"

// settingsDoc is the WHOLE settings document decoded as a generic map so
// every top-level key outside "hooks" survives the install/uninstall
// round trip untouched (a typed struct would silently drop unknown keys
// from the shared settings document). Only the hooks.Stop array is ever
// rewritten; key order after re-encode is alphabetical (cosmetic — the
// tests pin values, not key order).
type settingsDoc struct {
	doc map[string]any
}

func (d *settingsDoc) stopsRaw() (json.RawMessage, bool) {
	hooks, ok := d.doc["hooks"].(map[string]any)
	if !ok {
		return nil, false
	}
	raw, ok := hooks["Stop"]
	if !ok {
		return nil, false
	}
	enc, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return enc, true
}

func (d *settingsDoc) setStops(entries []stopHookEntry) error {
	hooks, ok := d.doc["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		d.doc["hooks"] = hooks
	}
	hooks["Stop"] = entries
	return nil
}

func (d *settingsDoc) deleteStops() {
	hooks, ok := d.doc["hooks"].(map[string]any)
	if !ok {
		return
	}
	delete(hooks, "Stop")
	if len(hooks) == 0 {
		delete(d.doc, "hooks")
	}
}

// stopHookEntry is one hook entry under hooks.Stop.
type stopHookEntry struct {
	Matcher string           `json:"matcher,omitempty"`
	Hooks   []stopHookAction `json:"hooks"`
}

type stopHookAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// stopHookMatcher is the matcher value our entry carries so the installer
// and uninstaller can identify their own entry among foreign Stop hooks.
const stopHookMatcher = "amq-remote"

// InstallStopHook opts in the user-level Stop hook in
// <home>/.claude/settings.json. Idempotent: a second call with our entry
// already present is a no-op returning the same settings path. Foreign
// hooks are never modified. The caller must pass an explicit opt-in; the
// installer never runs as a side effect of attach (architect ruling
// 10:59Z: ~/.claude is shared by every agent on this machine, and no live
// install happens without the sponsor's word).
func InstallStopHook(home string) (string, error) {
	path := filepath.Join(home, ".claude", "settings.json")
	return installStopHookAt(path)
}

func installStopHookAt(path string) (string, error) {
	doc, err := readSettings(path)
	if err != nil {
		return "", err
	}
	var stops []stopHookEntry
	if raw, ok := doc.stopsRaw(); ok {
		if err := json.Unmarshal(raw, &stops); err != nil {
			return "", fmt.Errorf("%s: hooks.Stop is not a hook list; refusing to edit shared settings: %w", path, err)
		}
	}
	for _, e := range stops {
		if entryIsOurs(e) {
			return path, nil // idempotent: already installed
		}
	}
	stops = append(stops, stopHookEntry{
		Matcher: stopHookMatcher,
		Hooks: []stopHookAction{{
			Type:    "command",
			Command: stopHookCommand,
			Timeout: 10,
		}},
	})
	if err := doc.setStops(stops); err != nil {
		return "", err
	}
	if err := writeSettings(path, doc); err != nil {
		return "", err
	}
	return path, nil
}

// UninstallStopHook removes our Stop-hook entry and restores the prior
// settings byte-for-byte (the pre-install bytes are returned by
// InstallStopHook's caller contract: uninstall restores from the file's
// own current state — it removes only OUR entry, leaving every other byte
// of every other value intact through the value-preserving round trip).
func UninstallStopHook(home string) (bool, error) {
	path := filepath.Join(home, ".claude", "settings.json")
	return uninstallStopHookAt(path)
}

func uninstallStopHookAt(path string) (bool, error) {
	doc, err := readSettings(path)
	if err != nil {
		return false, err
	}
	raw, ok := doc.stopsRaw()
	if !ok {
		return false, nil
	}
	var stops []stopHookEntry
	if err := json.Unmarshal(raw, &stops); err != nil {
		return false, fmt.Errorf("%s: hooks.Stop is not a hook list; refusing to edit shared settings: %w", path, err)
	}
	kept := stops[:0:0]
	removed := false
	for _, e := range stops {
		if entryIsOurs(e) {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		return false, nil
	}
	if len(kept) == 0 {
		doc.deleteStops()
	} else {
		if err := doc.setStops(kept); err != nil {
			return false, err
		}
	}
	if err := writeSettings(path, doc); err != nil {
		return false, err
	}
	return true, nil
}

func entryIsOurs(e stopHookEntry) bool {
	if e.Matcher != stopHookMatcher {
		return false
	}
	for _, h := range e.Hooks {
		if h.Type == "command" && h.Command == stopHookCommand {
			return true
		}
	}
	return false
}

func readSettings(path string) (*settingsDoc, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &settingsDoc{doc: map[string]any{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: not a JSON settings document; refusing to edit: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return &settingsDoc{doc: doc}, nil
}

// writeSettings preserves everything outside hooks.Stop: the document is
// decoded into the generic settingsFile, only the Stop array is replaced,
// and the result is re-encoded. A nil-Hooks document gets a hooks object
// only when installing.
func writeSettings(path string, doc *settingsDoc) error {
	out, err := json.MarshalIndent(doc.doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("prepare %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// restoreSettings is the byte-for-byte restore used by tests and by the
// installer's crash path: the caller captured the prior bytes with
// readSettingsRaw before any edit and can put them back exactly.
func readSettingsRaw(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func restoreSettings(path string, raw []byte, existed bool) error {
	if !existed {
		// The file did not exist before: remove the file we created.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(path, raw, 0o600)
}
