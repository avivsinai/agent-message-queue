package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureAgentPreservesUnmodelledKeys (611.22.55): EnsureAgent must not
// drop config.json keys the Config struct does not model. An operator
// hand-adds default_agent/project/wake/routing; a registration round-trip
// keeps them byte-identical.
func TestEnsureAgentPreservesUnmodelledKeys(t *testing.T) {
	rootDir := t.TempDir()
	metaDir := filepath.Join(rootDir, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{
  "version": 1,
  "created_utc": "2026-09-19T00:00:00Z",
  "agents": ["aviv"],
  "default_agent": "aviv",
  "project": "remote-control",
  "wake": {"interval_ms": 500},
  "extensions": {"remote": {"enabled": true}},
  "routing": {"fallback": "user"}
}`
	cfgPath := filepath.Join(metaDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	added, err := EnsureAgent(rootDir, "amit-pi-lead")
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if !added {
		t.Fatal("EnsureAgent reported no-op; handle should have been added")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("rewritten config is not valid JSON: %v\n%s", err, after)
	}
	// Every unmodelled key survives with identical raw bytes.
	for _, key := range []string{"default_agent", "project", "wake", "extensions", "routing"} {
		raw, ok := doc[key]
		if !ok {
			t.Fatalf("unmodelled key %q dropped by EnsureAgent rewrite\nbefore: %s\nafter: %s", key, original, after)
		}
		var want, got any
		if err := json.Unmarshal([]byte(original), &map[string]json.RawMessage{}); err != nil {
			t.Fatal(err)
		}
		var origDoc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(original), &origDoc); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(origDoc[key], &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(raw)) == "" {
			t.Fatalf("key %q empty", key)
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("key %q changed: want %s got %s", key, wantJSON, gotJSON)
		}
	}
	// Modelled keys updated correctly.
	if !strings.Contains(string(after), "amit-pi-lead") {
		t.Fatalf("handle missing after rewrite:\n%s", after)
	}
}

// TestEnsureAgentUnmodelledKeysNoOp pins the no-op arm: a handle already
// present does not rewrite the file at all (unknown keys untouched by
// definition, mtime-stable).
func TestEnsureAgentUnmodelledKeysNoOp(t *testing.T) {
	rootDir := t.TempDir()
	metaDir := filepath.Join(rootDir, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(metaDir, "config.json")
	original := []byte(`{"version":1,"created_utc":"x","agents":["amit-pi-lead"],"project":"p"}`)
	if err := os.WriteFile(cfgPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := EnsureAgent(rootDir, "amit-pi-lead")
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if added {
		t.Fatal("EnsureAgent reported added for an already-present handle")
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("no-op arm rewrote the file:\nbefore: %s\nafter: %s", original, after)
	}
}
