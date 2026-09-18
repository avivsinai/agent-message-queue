package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAbsentFileYieldsZeroAdapters(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "manifest.json"))
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	if len(f.Adapters) != 0 {
		t.Fatalf("got %d adapters, want 0", len(f.Adapters))
	}
	if f.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version=%d, want %d", f.SchemaVersion, SchemaVersion)
	}
}

func TestValidateRejectsDuplicateTarget(t *testing.T) {
	f := File{
		SchemaVersion: SchemaVersion,
		Adapters: []Adapter{
			{Kind: "fake", Target: "dup", Epoch: "e_1"},
			{Kind: "fake", Target: "dup", Epoch: "e_2"},
		},
	}
	err := Validate(f)
	if err == nil {
		t.Fatal("Validate accepted duplicate target")
	}
	dup, ok := err.(*ErrDuplicateTarget)
	if !ok {
		t.Fatalf("got %T, want *ErrDuplicateTarget", err)
	}
	if dup.Target != "dup" {
		t.Fatalf("dup target=%q, want %q", dup.Target, "dup")
	}
}

func TestValidateRejectsEpochOnNonFake(t *testing.T) {
	f := File{
		SchemaVersion: SchemaVersion,
		Adapters: []Adapter{
			{Kind: "codex", Target: "cx-1", Epoch: "cx-123"},
		},
	}
	err := Validate(f)
	if err == nil {
		t.Fatal("Validate accepted epoch on non-fake")
	}
	e, ok := err.(*ErrEpochOnNonFake)
	if !ok {
		t.Fatalf("got %T, want *ErrEpochOnNonFake", err)
	}
	if e.Kind != "codex" {
		t.Fatalf("kind=%q, want codex", e.Kind)
	}
}

func TestLoadValidManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	cfg, _ := json.Marshal(map[string]string{"socket": "/tmp/x", "thread": "t1"})
	doc := File{
		SchemaVersion: SchemaVersion,
		Adapters: []Adapter{
			{Kind: "fake", Target: "fake", Epoch: "e_1"},
			{Kind: "codex", Target: "cx-1", Config: cfg},
		},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Adapters) != 2 {
		t.Fatalf("got %d adapters, want 2", len(f.Adapters))
	}
}
