package launch

import (
	"encoding/json"
	"testing"
)

// TestWriteApplySessionConfigPreservesUnmodelledKeys pins 611.22.57 (the
// setup/apply half of review-821-r1 P1-b): the apply roster write must not
// wipe unmodelled keys (default_agent/project/routing) that exist in a live
// root's meta/config.json. The pre-fix code re-marshaled a bare struct.
func TestWriteApplySessionConfigPreservesUnmodelledKeys(t *testing.T) {
	_, root := openTestRoot(t)
	if err := root.EnsureRootDirs(); err != nil {
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
	if _, err := root.WriteFileAtomic("meta", "config.json", []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(root, "test-nonce-611-22-57")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	if err := writeApplySessionConfig(root, lease, "2026-09-19T12:00:00Z", []string{"claude", "codex"}); err != nil {
		t.Fatalf("writeApplySessionConfig: %v", err)
	}

	after, readErr := root.ReadFile("meta/config.json")
	if readErr != nil {
		t.Fatal(readErr)
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
			t.Fatalf("unmodelled key %q dropped by apply roster write\nbefore: %s\nafter: %s", key, original, after)
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
	// Modelled keys still take the struct's values.
	var agents []string
	if err := json.Unmarshal(doc["agents"], &agents); err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0] != "claude" || agents[1] != "codex" {
		t.Fatalf("agents = %v, want [claude codex]", agents)
	}
	if string(doc["created_utc"]) != `"2026-09-19T12:00:00Z"` {
		t.Fatalf("created_utc = %s, want the passed value", doc["created_utc"])
	}
}
