package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetupPreservesUnmodelledConfigKeys pins 611.22.57 (the setup/apply
// half of review-821-r1 P1-b): a config.json that an operator hand-enriched
// with unmodelled keys (default_agent/project/routing) survives `amq setup`
// byte-identical in value. Live-proven failure: immediately after a
// registration preserved the keys, `amq setup` wiped them.
func TestSetupPreservesUnmodelledConfigKeys(t *testing.T) {
	project := setupProjectFixture(t, "claude", "codex", "grok")
	cfgPath := filepath.Join(project, ".agent-mail", "meta", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{
  "version": 1,
  "created_utc": "2026-09-19T00:00:00Z",
  "agents": ["claude"],
  "default_agent": "claude",
  "project": "remote-control",
  "routing": {"fallback": "user"}
}`
	if err := os.WriteFile(cfgPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	setupLookPath = func(name string) (string, error) {
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { setupLookPath = execLookPathForSetup })

	if _, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude,codex", "--default-session", "work", "--launcher-preference", "tmux", "--json"})
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc, origDoc map[string]json.RawMessage
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("rewritten config is not valid JSON: %v\n%s", err, after)
	}
	if err := json.Unmarshal([]byte(original), &origDoc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"default_agent", "project", "routing"} {
		var want, got any
		if err := json.Unmarshal(origDoc[key], &want); err != nil {
			t.Fatal(err)
		}
		raw, ok := doc[key]
		if !ok {
			t.Fatalf("unmodelled key %q dropped by setup rewrite\nbefore: %s\nafter: %s", key, original, after)
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("key %q changed: want %s got %s", key, wantJSON, gotJSON)
		}
	}
	if !strings.Contains(string(after), "codex") {
		t.Fatalf("roster union did not add the new handle:\n%s", after)
	}
}
