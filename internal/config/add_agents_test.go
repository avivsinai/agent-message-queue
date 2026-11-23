package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

func seedConfig(t *testing.T, root, doc string) string {
	t.Helper()
	path := filepath.Join(root, "meta", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type provisionCall struct {
	handle     string
	registered bool
}

func TestAddAgentsMergesPreservingFieldsAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	path := seedConfig(t, root, `{"agents":["codex","claude"],"created_utc":"2026-01-01T00:00:00Z","default_agent":"codex","version":1}`)

	var calls []provisionCall
	provision := func(handle string, registered bool) error {
		calls = append(calls, provisionCall{handle, registered})
		return nil
	}
	added, err := AddAgents(root, []string{"alpha", "claude", "beta"}, provision)
	if err != nil {
		t.Fatalf("AddAgents: %v", err)
	}
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(added, want) {
		t.Fatalf("added = %v, want %v", added, want)
	}
	wantCalls := []provisionCall{{"alpha", false}, {"claude", true}, {"beta", false}}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("provision calls = %v, want %v", calls, wantCalls)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version      int      `json:"version"`
		CreatedUTC   string   `json:"created_utc"`
		Agents       []string `json:"agents"`
		DefaultAgent string   `json:"default_agent"`
	}
	if err := json.Unmarshal(written, &doc); err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex", "claude", "alpha", "beta"}; !reflect.DeepEqual(doc.Agents, want) {
		t.Fatalf("agents = %v, want %v", doc.Agents, want)
	}
	if doc.Version != 1 || doc.CreatedUTC != "2026-01-01T00:00:00Z" || doc.DefaultAgent != "codex" {
		t.Fatalf("preserved fields changed: %s", written)
	}

	again, err := AddAgents(root, []string{"beta", "alpha"}, nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("repeat AddAgents = %v, %v; want nothing added", again, err)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, rewritten) {
		t.Fatalf("repeat AddAgents rewrote config:\nbefore: %s\nafter:  %s", written, rewritten)
	}
}

// The roster must never name a handle whose mailbox provisioning failed.
func TestAddAgentsProvisionFailureLeavesRosterUnchanged(t *testing.T) {
	root := t.TempDir()
	seed := `{"agents":["codex"],"created_utc":"2026-01-01T00:00:00Z","version":1}`
	path := seedConfig(t, root, seed)
	wantErr := errors.New("mkdir failed")
	_, err := AddAgents(root, []string{"alpha", "beta"}, func(handle string, _ bool) error {
		if handle == "beta" {
			return wantErr
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("AddAgents = %v, want %v", err, wantErr)
	}
	if got, _ := os.ReadFile(path); string(got) != seed {
		t.Fatalf("config changed after a failed provision: %s", got)
	}
}

func TestAddAgentsRequiresExistingConfig(t *testing.T) {
	root := t.TempDir()
	_, err := AddAgents(root, []string{"alpha"}, func(string, bool) error {
		t.Fatal("provision called without a config")
		return nil
	})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("AddAgents = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(filepath.Join(root, "meta", "config.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("AddAgents created a config: %v", err)
	}
}

func TestAddAgentsConcurrentNoLostRegistration(t *testing.T) {
	root := t.TempDir()
	path := seedConfig(t, root, `{"agents":["codex"],"created_utc":"2026-01-01T00:00:00Z","version":1}`)
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(handle string) {
			defer wg.Done()
			if _, err := AddAgents(root, []string{handle, "shared"}, nil); err != nil {
				errs <- err
			}
		}("agent-" + strconv.Itoa(i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent AddAgents: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Agents) != workers+2 {
		t.Fatalf("agents = %v, want codex, shared, and %d workers once each", loaded.Agents, workers)
	}
	seen := map[string]bool{}
	for _, a := range loaded.Agents {
		if seen[a] {
			t.Fatalf("duplicate %q in %v", a, loaded.Agents)
		}
		seen[a] = true
	}
	for i := 0; i < workers; i++ {
		if !seen["agent-"+strconv.Itoa(i)] {
			t.Fatalf("agent-%d lost: %v", i, loaded.Agents)
		}
	}
}
