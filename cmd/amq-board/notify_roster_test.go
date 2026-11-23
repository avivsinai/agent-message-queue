package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

type notifyFixtureRunner struct{ root, session string }

func (r notifyFixtureRunner) Run(_ context.Context, _ string, argv []string) ([]byte, error) {
	switch fmt.Sprint(argv) {
	case "[amq env --json]":
		return json.Marshal(map[string]string{"root": r.root, "root_id": "id", "base_root": r.root, "base_root_id": "baseid", "session_name": r.session, "me": "floor"})
	case "[amq who --json]":
		return json.Marshal([]map[string]any{{"name": "", "scope": "base", "root": r.root, "agents": []map[string]string{{"handle": "floor"}, {"handle": "lead"}, {"handle": "alt"}, {"handle": "-invalid"}}}, {"name": "other", "scope": "session", "root": filepath.Join(r.root, "other"), "agents": []map[string]string{{"handle": "foreign"}}}})
	default:
		return nil, fmt.Errorf("unexpected notify source command: %v", argv)
	}
}
func (r notifyFixtureRunner) RunLimited(ctx context.Context, dir string, argv []string, _ int) ([]byte, error) {
	return r.Run(ctx, dir, argv)
}

// amq who returns an array, including a base scope when no session is active.
func TestNotifyRosterBaseOnlyProject(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roster, err := notifyRoster(context.Background(), board.Env{Run: notifyFixtureRunner{root: root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(roster) != 3 {
		t.Fatalf("base roster = %+v", roster)
	}
	for _, entry := range roster {
		if entry.Session != "" || !entry.ValidHandle {
			t.Fatalf("base scope = %+v", entry)
		}
	}
}
