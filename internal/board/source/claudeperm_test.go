package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

type claudePermRunner func([]string) ([]byte, error)

func (r claudePermRunner) Run(_ context.Context, _ string, argv []string) ([]byte, error) {
	return r(argv)
}

func (r claudePermRunner) RunLimited(_ context.Context, _ string, argv []string, _ int) ([]byte, error) {
	return r(argv)
}

// Slice 5a: a captured, scrubbed Claude status file places a permission
// wait on its mapped agent, or marks a cwd owner join inferred. Design
// finding 6 excludes foreign payload, and finding 8 keeps old mapped waits.
// The new version row: amq-board refused every status record of the
// installed Claude Code 2.1.312 as "not supported" (2026-10-29), so its
// permission waits did not show.
func TestClaudePermissionWaitsFromSessionStatus(t *testing.T) {
	fixture, err := os.ReadFile("testdata/claude-session-wait-2.1.308.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mapped old wait", "inferred owner", "foreign project", "new version", "old discovery", "other dialog", "duplicate session", "dead process", "nongit project"} {
		t.Run(name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			if err := os.Mkdir(filepath.Join(home, "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(fixture, &record); err != nil {
				t.Fatal(err)
			}
			const id = "00000000-0000-4000-8000-000000000001"
			pid := os.Getpid()
			if name == "dead process" {
				// Claude review of slice 5a: a crashed process left a mapped
				// permission wait behind. This PID does not name a live process.
				pid = 1<<31 - 1
			}
			record["pid"] = pid
			record["cwd"] = project
			if name == "foreign project" {
				record["cwd"] = t.TempDir()
				// A foreign payload must never reach its decoder or the model.
				record["name"] = map[string]string{"private": "FOREIGN_PAYLOAD"}
				record["status"] = []string{"FOREIGN_PAYLOAD"}
			}
			if name == "new version" {
				record["version"] = "2.1.312"
			}
			if name == "other dialog" {
				record["waitingFor"] = "dialog open"
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "sessions", strconv.Itoa(pid)+".json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.UnixMilli(1800000000000).Add(48 * time.Hour)
			mtime := now
			if name == "mapped old wait" || name == "old discovery" {
				mtime = now.Add(-48 * time.Hour)
			}
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			// Claude review of slice 5a: a resumed session can leave two PID
			// files with one sessionId. Only the newest status may contribute.
			if name == "duplicate session" {
				record["pid"] = os.Getppid()
				record["statusUpdatedAt"] = int64(1800000000001)
				record["name"] = "newest"
				newest, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				newestPath := filepath.Join(home, "sessions", strconv.Itoa(os.Getppid())+".json")
				if err := os.WriteFile(newestPath, newest, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(newestPath, now, now); err != nil {
					t.Fatal(err)
				}
			}
			var conversations []map[string]string
			if name == "mapped old wait" || name == "dead process" {
				conversations = append(conversations, map[string]string{"handle": "reviewer", "provider": "claude", "id": id, "state": "ready"})
			}
			listing, _ := json.Marshal(map[string]any{"sessions": []any{map[string]any{"name": "s1", "conversations": conversations}}})
			runner := claudePermRunner(func(argv []string) ([]byte, error) {
				switch strings.Join(argv, " ") {
				case "amq session list --json":
					return listing, nil
				case "amq who --json":
					return []byte(`[{"name":"s1","agents":[{"handle":"reviewer","active":true}]}]`), nil
				case "git worktree list --porcelain -z":
					// Board state/scope review: installed 0.111.0 degraded this
					// source outside Git instead of reading the exact project directory.
					if name == "nongit project" {
						return nil, fmt.Errorf("git: fatal: not a git repository (or any of the parent directories): .git")
					}
					return []byte("worktree " + project + "\x00\x00"), nil
				}
				return nil, fmt.Errorf("unexpected command %v", argv)
			})
			env := board.Env{Dir: project, Now: func() time.Time { return now }, Run: runner}
			got := (ClaudePermissions{}).Read(context.Background(), env)
			if got.Status != board.StatusOK {
				t.Fatalf("read: %+v", got)
			}
			if name == "new version" && got.Version != "2.1.312" {
				t.Fatalf("producer version: %q", got.Version)
			}
			if name == "foreign project" || name == "old discovery" || name == "other dialog" || name == "dead process" {
				if len(got.Items) != 0 {
					t.Fatalf("unexpected items: %+v", got.Items)
				}
				if name == "old discovery" && got.Coverage.Skipped != 1 {
					t.Fatalf("old discovery coverage: %+v", got.Coverage)
				}
				return
			}
			if len(got.Items) != 1 {
				t.Fatalf("items = %+v, want one wait", got.Items)
			}
			it := got.Items[0]
			if it.Ref.String() != "claude-session:"+id || it.Owner.Handle != "reviewer" || it.Owner.Session != "s1" || it.Structural != (name == "mapped old wait") {
				t.Fatalf("owner/ref: %+v", it)
			}
			waitAt := int64(1800000000000)
			if name == "duplicate session" {
				waitAt++
				if !strings.Contains(it.Title, "newest") {
					t.Fatalf("older duplicate won: %+v", it)
				}
			}
			if !it.Reason.Since.Equal(time.UnixMilli(waitAt)) || it.WaitsOn[0].Kind != board.OwnerHuman {
				t.Fatalf("wait reason: %+v", it)
			}
			model := board.Build(board.Input{Now: now, Config: board.DefaultConfig(), Results: []board.NamedResult{{Name: "claude-permissions", Result: got}}})
			cards := model.Columns[0].Cards
			if model.Columns[0].ID != board.ColumnWaiting || len(cards) != 1 || slices.Contains(cards[0].Marks, "inferred (owner)") != (name != "mapped old wait") {
				t.Fatalf("waiting column: %+v", model.Columns[0])
			}
		})
	}
}
