package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestMarshalPreservingUnknownsEmptyOriginalIsSortedAndStable pins the
// 611.22.57 no-op invariant: a fresh write (empty original) and a re-read
// rewrite of that same file must produce byte-identical output. Sorted-map
// output everywhere makes the first write stable under rewrite; struct-order
// output on the empty-original path broke a matching `amq setup` rerun.
func TestMarshalPreservingUnknownsEmptyOriginalIsSortedAndStable(t *testing.T) {
	cfg := Config{Version: 1, CreatedUTC: "2026-09-19T00:00:00Z"}
	cfg.Agents = []string{"claude", "codex"}

	fresh, err := MarshalPreservingUnknowns(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) == 0 || fresh[len(fresh)-1] != '}' {
		t.Fatalf("fresh marshal is not a JSON object: %q", fresh)
	}

	rewritten, err := MarshalPreservingUnknowns(fresh, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh, rewritten) {
		t.Fatalf("rewrite not byte-stable\nfresh (%d): %s\nrewritten (%d): %s", len(fresh), fresh, len(rewritten), rewritten)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(fresh, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["version"]; !ok {
		t.Fatalf("modelled key version missing: %s", fresh)
	}
	if _, ok := doc["agents"]; !ok {
		t.Fatalf("modelled key agents missing: %s", fresh)
	}
}
