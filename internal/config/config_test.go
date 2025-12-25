package config

import (
	"path/filepath"
	"testing"
)

func TestConfigWriteRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "meta", "config.json")
	cfg := Config{
		Version:    1,
		CreatedUTC: "2025-12-24T15:02:33Z",
		Agents:     []string{"codex", "cloudcode"},
	}
	if err := WriteConfig(path, cfg, false); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.Agents) != 2 || loaded.Agents[0] != "codex" {
		t.Fatalf("unexpected agents: %+v", loaded.Agents)
	}
	if err := WriteConfig(path, cfg, false); err == nil {
		t.Fatalf("expected error on overwrite without force")
	}
	if err := WriteConfig(path, cfg, true); err != nil {
		t.Fatalf("WriteConfig with force: %v", err)
	}
}
