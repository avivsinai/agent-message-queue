package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

type codexGoalsRunner func(context.Context, []string) ([]byte, error)

func (r codexGoalsRunner) Run(ctx context.Context, _ string, argv []string) ([]byte, error) {
	return r(ctx, argv)
}

func (r codexGoalsRunner) RunLimited(ctx context.Context, _ string, argv []string, _ int) ([]byte, error) {
	return r(ctx, argv)
}

// Slice 5b: one scrubbed store protects state mapping and project payload
// eligibility. The foreign-project row is the design's finding 6 regression;
// unknown file versions must not be interpreted as the supported schema.
func TestCodexGoalsReadVersionedStore(t *testing.T) {
	db, err := os.ReadFile("testdata/codex-goals_1.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project goals", "foreign project", "unknown version", "busy database"} {
		t.Run(name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			t.Setenv("CODEX_HOME", home)
			file := "goals_1.sqlite"
			if name == "unknown version" {
				file = "goals_2.sqlite"
			}
			if err := os.WriteFile(filepath.Join(home, file), db, 0o600); err != nil {
				t.Fatal(err)
			}
			cwd := project
			if name == "foreign project" {
				cwd = t.TempDir()
			}
			meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": "019f0000-0000-7000-8000-000000000001", "cwd": cwd}})
			if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "sessions", "rollout-test-019f0000-0000-7000-8000-000000000001.jsonl"), append(meta, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			var conversations []map[string]string
			if name == "project goals" || name == "busy database" {
				for i := 2; i <= 6; i++ {
					conversations = append(conversations, map[string]string{"handle": "codex", "provider": "codex", "id": fmt.Sprintf("019f0000-0000-7000-8000-%012d", i), "state": "ready"})
				}
			}
			listing, _ := json.Marshal(map[string]any{"sessions": []any{map[string]any{"name": "s1", "conversations": conversations}}})
			queries := 0
			runner := codexGoalsRunner(func(ctx context.Context, argv []string) ([]byte, error) {
				switch argv[0] {
				case "amq":
					if argv[1] == "session" {
						return listing, nil
					}
					return []byte(`[{"name":"s1","agents":[{"handle":"codex","active":true}]}]`), nil
				case "git":
					return []byte("worktree " + project + "\x00\x00"), nil
				case "sqlite3":
					queries++
					if name == "busy database" {
						return nil, fmt.Errorf("database is locked")
					}
					if name != "project goals" {
						t.Fatal("ineligible goal payload selected")
					}
					joined := strings.Join(argv, " ")
					if !strings.Contains(joined, "-readonly") || !strings.Contains(joined, "mode=ro") || strings.Contains(joined, "immutable") {
						t.Fatal("query must use a live read-only connection")
					}
					query := argv[len(argv)-1]
					if !strings.Contains(query, "SELECT thread_id, objective, status, updated_at_ms FROM thread_goals WHERE thread_id IN (") {
						t.Fatal("query must project named fields for eligible threads")
					}
					sqlite, err := exec.LookPath("sqlite3")
					if err != nil {
						if errors.Is(err, exec.ErrNotFound) {
							t.Skip("sqlite3 not found")
						}
						t.Fatal(err)
					}
					// PR #1202 review: the happy path must execute the actual query,
					// so a wrong table or column fails against the fixture schema.
					output, err := exec.CommandContext(ctx, sqlite, argv[1:]...).CombinedOutput()
					if err != nil {
						return nil, fmt.Errorf("sqlite3 fixture query: %w: %s", err, output)
					}
					return output, nil
				}
				return nil, fmt.Errorf("unexpected command %v", argv)
			})
			env := board.Env{Dir: project, Now: func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }, Run: runner}
			got := (CodexGoals{}).Read(context.Background(), env)
			if name == "unknown version" {
				if got.Status != board.StatusDegraded || !strings.Contains(got.Detail, "not supported") || queries != 0 {
					t.Fatalf("unknown version result: %+v", got)
				}
				return
			}
			if name == "busy database" {
				if got.Status != board.StatusDegraded || !strings.Contains(got.Detail, "database is locked") || len(got.Items) != 0 {
					t.Fatalf("busy database result: %+v", got)
				}
				return
			}
			if got.Status != board.StatusOK {
				t.Fatalf("read: %+v", got)
			}
			if name == "foreign project" {
				if len(got.Items) != 0 || queries != 0 {
					t.Fatal("foreign project contributed payload")
				}
				return
			}
			want := []board.State{board.StateActive, board.StateBlocked, board.StatePlanned, board.StateBlocked, board.StateBlocked, board.StateDone}
			if queries != 1 || len(got.Items) != len(want) {
				t.Fatalf("queries=%d items=%+v", queries, got.Items)
			}
			for i, it := range got.Items {
				if it.State != want[i] || it.Owner.Handle != "codex" || it.Owner.Session != "s1" || it.Structural != (i > 0) || it.Ref.String() != fmt.Sprintf("codex-goal:019f0000-0000-7000-8000-%012d", i+1) {
					t.Errorf("item %d: %+v", i, it)
				}
			}
		})
	}
}
