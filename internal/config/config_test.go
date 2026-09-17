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
		Agents:     []string{"codex", "claude"},
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

func TestEnsureAgentPreservesExisting(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "meta", "config.json")
	// Seed with existing agents.
	if err := WriteConfig(path, Config{
		Version:    1,
		CreatedUTC: "2026-01-01T00:00:00Z",
		Agents:     []string{"codex", "claude"},
	}, true); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Add the remote handle: existing agents MUST be preserved.
	added, err := EnsureAgent(path, "remote")
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if !added {
		t.Fatal("added=false, want true (handle was new)")
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.Agents) != 3 {
		t.Fatalf("agents=%v, want 3 (codex, claude, remote)", loaded.Agents)
	}
	// The original agents survive.
	has := func(h string) bool {
		for _, a := range loaded.Agents {
			if a == h {
				return true
			}
		}
		return false
	}
	if !has("codex") || !has("claude") {
		t.Fatalf("existing agents lost: %v", loaded.Agents)
	}
	if !has("remote") {
		t.Fatalf("remote handle not added: %v", loaded.Agents)
	}
}

func TestEnsureAgentIdempotent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "meta", "config.json")
	if err := WriteConfig(path, Config{
		Version: 1, CreatedUTC: "2026-01-01T00:00:00Z",
		Agents: []string{"codex", "remote"},
	}, true); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Already present: no-op, no duplicate.
	added, err := EnsureAgent(path, "remote")
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if added {
		t.Fatal("added=true, want false (already present)")
	}
	loaded, _ := LoadConfig(path)
	count := 0
	for _, a := range loaded.Agents {
		if a == "remote" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("remote appears %d times, want 1", count)
	}
}

func TestEnsureAgentCreatesConfig(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "meta", "config.json")
	// No config exists: EnsureAgent creates one with this handle.
	added, err := EnsureAgent(path, "remote")
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if !added {
		t.Fatal("added=false, want true (config was created)")
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.Agents) != 1 || loaded.Agents[0] != "remote" {
		t.Fatalf("agents=%v, want [remote]", loaded.Agents)
	}
}
