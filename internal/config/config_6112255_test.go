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
// preserves them semantically (values round-trip intact; the file is
// re-serialized with sorted keys and 2-space indentation).
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
	var origDoc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(original), &origDoc); err != nil {
		t.Fatal(err)
	}
	// Every unmodelled key survives semantically: compare decoded values,
	// not bytes (the rewrite re-serializes the whole document).
	for _, key := range []string{"default_agent", "project", "wake", "extensions", "routing"} {
		raw, ok := doc[key]
		if !ok {
			t.Fatalf("unmodelled key %q dropped by EnsureAgent rewrite\nbefore: %s\nafter: %s", key, original, after)
		}
		var want, got any
		if err := json.Unmarshal(origDoc[key], &want); err != nil {
			t.Fatal(err)
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
	// Modelled keys updated correctly.
	if !strings.Contains(string(after), "amit-pi-lead") {
		t.Fatalf("handle missing after rewrite:\n%s", after)
	}
}

// TestEnsureAgentNullDocumentNoPanic pins review-821-r1 P1-a: a literal
// JSON `null` document unmarshals into a nil map with no error; the
// preservation overlay must treat it as an empty document, not panic.
// origin/main returned nil error here; the recut keeps that contract.
func TestEnsureAgentNullDocumentNoPanic(t *testing.T) {
	rootDir := t.TempDir()
	metaDir := filepath.Join(rootDir, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(metaDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := EnsureAgent(rootDir, "amit-pi-lead")
	if err != nil {
		t.Fatalf("EnsureAgent(null document): %v", err)
	}
	if !added {
		t.Fatal("EnsureAgent reported no-op on a null document; handle should have been added")
	}
	var doc map[string]json.RawMessage
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("rewritten config is not valid JSON: %v\n%s", err, after)
	}
	if len(doc["agents"]) == 0 || !strings.Contains(string(doc["agents"]), "amit-pi-lead") {
		t.Fatalf("agents missing the new handle after null-document rewrite:\n%s", after)
	}
}
