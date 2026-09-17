package config

import (
	"os"
	"path/filepath"
	"sync"
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
	added, err := EnsureAgent(root, "remote")
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
	added, err := EnsureAgent(root, "remote")
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
	added, err := EnsureAgent(root, "remote")
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

// TestEnsureAgentB1RejectsInvalidHandle is the regression test for the round-1
// review blocker B1 (611.22.19 round-2): a bad --me handle was written into
// config.json BEFORE validation, poisoning the root. EnsureAgent now calls
// fsq.ValidateHandle at the top, before any write, so an invalid handle never
// lands. The config is NOT created for a bad handle.
func TestEnsureAgentB1RejectsInvalidHandle(t *testing.T) {
	for _, bad := range []string{"Bad", "../escape", "-bad", ""} {
		t.Run(bad, func(t *testing.T) {
			root := t.TempDir()
			added, err := EnsureAgent(root, bad)
			if err == nil {
				t.Fatalf("EnsureAgent(%q): want error, got nil (added=%v)", bad, added)
			}
			// No config must have been written.
			if _, err := os.Stat(filepath.Join(root, "meta", "config.json")); err == nil {
				t.Fatalf("EnsureAgent(%q): config.json was created (poisoned root)", bad)
			}
		})
	}
}

// TestEnsureAgentB2ConcurrentNoLostRegistration is the regression test for the
// round-1 review blocker B2 (611.22.19 round-2): EnsureAgent was an unguarded
// read-modify-write; two concurrent calls with different handles lost one
// registration 200/200 times. Now it uses the guarded seam
// (OpenMailboxConfigAuthorization + Verify + DeliveryRoot write).
func TestEnsureAgentB2ConcurrentNoLostRegistration(t *testing.T) {
	root := t.TempDir()
	// Seed with an initial config.
	path := filepath.Join(root, "meta", "config.json")
	if err := WriteConfig(path, Config{
		Version: 1, CreatedUTC: "2026-01-01T00:00:00Z",
		Agents: []string{"codex"},
	}, true); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = EnsureAgent(root, "claude")
	}()
	go func() {
		defer wg.Done()
		_, _ = EnsureAgent(root, "remote")
	}()
	wg.Wait()
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Both handles must be present (plus the original codex).
	has := func(h string) bool {
		for _, a := range loaded.Agents {
			if a == h {
				return true
			}
		}
		return false
	}
	if !has("codex") {
		t.Fatalf("original agent lost: %v", loaded.Agents)
	}
	if !has("claude") {
		t.Fatalf("claude lost in concurrent registration: %v", loaded.Agents)
	}
	if !has("remote") {
		t.Fatalf("remote lost in concurrent registration: %v", loaded.Agents)
	}
}
