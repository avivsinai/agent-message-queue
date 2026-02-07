package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/swarm"
)

func setupClaudeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// os.UserHomeDir honors different env vars per platform.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func writeTeamConfig(t *testing.T, home, team string, cfg map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "teams", team)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTasksList(t *testing.T, home, team string, tasks []map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "tasks", team)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := map[string]any{"tasks": tasks}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "tasks.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	runErr := fn()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	_ = r.Close()

	return buf.String(), runErr
}

func TestRunSwarm_ListJoinLeaveTasksClaimComplete_JSON(t *testing.T) {
	home := setupClaudeHome(t)

	team := "my-team"
	writeTeamConfig(t, home, team, map[string]any{
		"name": team,
		"members": []any{
			map[string]any{
				"name":       "claude",
				"agent_id":   "cc-1",
				"agent_type": swarm.AgentTypeClaudeCode,
			},
		},
	})

	writeTasksList(t, home, team, []map[string]any{
		{"id": "t1", "title": "First", "status": swarm.TaskStatusPending},
		{"id": "t2", "title": "Second", "status": swarm.TaskStatusCompleted},
	})

	t.Run("list", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return runSwarmList([]string{"--json"})
		})
		if err != nil {
			t.Fatalf("runSwarmList: %v", err)
		}
		var teams []swarm.TeamSummary
		if err := json.Unmarshal([]byte(out), &teams); err != nil {
			t.Fatalf("unmarshal: %v (output: %s)", err, out)
		}
		if len(teams) != 1 {
			t.Fatalf("len(teams) = %d, want 1", len(teams))
		}
		if teams[0].Name != team {
			t.Fatalf("team name = %q, want %q", teams[0].Name, team)
		}
		if teams[0].MemberCount != 1 {
			t.Fatalf("member count = %d, want 1", teams[0].MemberCount)
		}
	})

	t.Run("join", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return runSwarmJoin([]string{
				"--team", team,
				"--me", "codex",
				"--type", swarm.AgentTypeCodex,
				"--agent-id", "ext_codex_1",
				"--json",
			})
		})
		if err != nil {
			t.Fatalf("runSwarmJoin: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("unmarshal: %v (output: %s)", err, out)
		}
		if got["team"] != team {
			t.Fatalf("team = %v, want %q", got["team"], team)
		}
		if got["name"] != "codex" {
			t.Fatalf("name = %v, want %q", got["name"], "codex")
		}
		if got["agent_id"] != "ext_codex_1" {
			t.Fatalf("agent_id = %v, want %q", got["agent_id"], "ext_codex_1")
		}
	})

	t.Run("tasks", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return runSwarmTasks([]string{"--team", team, "--json"})
		})
		if err != nil {
			t.Fatalf("runSwarmTasks: %v", err)
		}
		var tasks []swarm.Task
		if err := json.Unmarshal([]byte(out), &tasks); err != nil {
			t.Fatalf("unmarshal: %v (output: %s)", err, out)
		}
		if len(tasks) != 2 {
			t.Fatalf("len(tasks) = %d, want 2", len(tasks))
		}
	})

	t.Run("tasks filter", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return runSwarmTasks([]string{"--team", team, "--status", swarm.TaskStatusCompleted, "--json"})
		})
		if err != nil {
			t.Fatalf("runSwarmTasks(filter): %v", err)
		}
		var tasks []swarm.Task
		if err := json.Unmarshal([]byte(out), &tasks); err != nil {
			t.Fatalf("unmarshal: %v (output: %s)", err, out)
		}
		if len(tasks) != 1 {
			t.Fatalf("len(tasks) = %d, want 1", len(tasks))
		}
		if tasks[0].ID != "t2" {
			t.Fatalf("task id = %q, want %q", tasks[0].ID, "t2")
		}
	})

	t.Run("claim and complete", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return runSwarmClaim([]string{"--team", team, "--task", "t1", "--me", "codex", "--json"})
		})
		if err != nil {
			t.Fatalf("runSwarmClaim: %v", err)
		}
		var claim map[string]any
		if err := json.Unmarshal([]byte(out), &claim); err != nil {
			t.Fatalf("unmarshal claim: %v (output: %s)", err, out)
		}
		if claim["assigned_to"] != "ext_codex_1" {
			t.Fatalf("assigned_to = %v, want %q", claim["assigned_to"], "ext_codex_1")
		}
		if claim["status"] != swarm.TaskStatusInProgress {
			t.Fatalf("status = %v, want %q", claim["status"], swarm.TaskStatusInProgress)
		}

		out, err = captureStdout(t, func() error {
			return runSwarmComplete([]string{"--team", team, "--task", "t1", "--me", "codex", "--json"})
		})
		if err != nil {
			t.Fatalf("runSwarmComplete: %v", err)
		}
		var complete map[string]any
		if err := json.Unmarshal([]byte(out), &complete); err != nil {
			t.Fatalf("unmarshal complete: %v (output: %s)", err, out)
		}
		if complete["status"] != swarm.TaskStatusCompleted {
			t.Fatalf("status = %v, want %q", complete["status"], swarm.TaskStatusCompleted)
		}
	})

	t.Run("leave", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return runSwarmLeave([]string{"--team", team, "--agent-id", "ext_codex_1", "--json"})
		})
		if err != nil {
			t.Fatalf("runSwarmLeave: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("unmarshal: %v (output: %s)", err, out)
		}
		if got["removed"] != true {
			t.Fatalf("removed = %v, want true", got["removed"])
		}
	})
}

func TestRunSwarmClaim_DependencyGating_ReturnsError(t *testing.T) {
	home := setupClaudeHome(t)
	team := "dep-team"

	writeTeamConfig(t, home, team, map[string]any{
		"name": team,
		"members": []any{
			map[string]any{"name": "codex", "agent_id": "ext_codex_1", "agent_type": swarm.AgentTypeCodex},
		},
	})

	writeTasksList(t, home, team, []map[string]any{
		{"id": "t1", "title": "Prereq", "status": swarm.TaskStatusPending},
		{"id": "t2", "title": "Blocked", "status": swarm.TaskStatusPending, "depends_on": []any{"t1"}},
	})

	_, err := captureStdout(t, func() error {
		return runSwarmClaim([]string{"--team", team, "--task", "t2", "--me", "codex"})
	})
	if err == nil {
		t.Fatal("expected error for blocked task")
	}
	if got := err.Error(); got == "" || !bytes.Contains([]byte(got), []byte("blocked by incomplete dependencies")) {
		t.Fatalf("unexpected error: %v", err)
	}

	// Ensure task is still pending/unassigned.
	tasks, listErr := swarm.ListTasks(team)
	if listErr != nil {
		t.Fatalf("ListTasks: %v", listErr)
	}
	for _, tt := range tasks {
		if tt.ID != "t2" {
			continue
		}
		if tt.Status != swarm.TaskStatusPending {
			t.Fatalf("t2 status = %q, want %q", tt.Status, swarm.TaskStatusPending)
		}
		if tt.AssignedTo != "" {
			t.Fatalf("t2 assigned_to = %q, want empty", tt.AssignedTo)
		}
	}
}

// --- Error-path tests ---

func TestRunSwarmJoin_MissingTeam(t *testing.T) {
	_ = setupClaudeHome(t)
	_, err := captureStdout(t, func() error {
		return runSwarmJoin([]string{"--me", "codex"})
	})
	if err == nil {
		t.Fatal("expected error for missing --team")
	}
}

func TestRunSwarmJoin_InvalidHandle(t *testing.T) {
	home := setupClaudeHome(t)
	writeTeamConfig(t, home, "t", map[string]any{"name": "t", "members": []any{}})
	_, err := captureStdout(t, func() error {
		return runSwarmJoin([]string{"--team", "t", "--me", "INVALID!"})
	})
	if err == nil {
		t.Fatal("expected error for invalid handle")
	}
}

func TestRunSwarmJoin_InvalidType(t *testing.T) {
	home := setupClaudeHome(t)
	writeTeamConfig(t, home, "t", map[string]any{"name": "t", "members": []any{}})
	_, err := captureStdout(t, func() error {
		return runSwarmJoin([]string{"--team", "t", "--me", "codex", "--type", "bogus"})
	})
	if err == nil {
		t.Fatal("expected error for invalid --type value")
	}
}

func TestRunSwarmLeave_MissingFlags(t *testing.T) {
	_ = setupClaudeHome(t)
	_, err := captureStdout(t, func() error {
		return runSwarmLeave([]string{})
	})
	if err == nil {
		t.Fatal("expected error for missing --team")
	}
	_, err = captureStdout(t, func() error {
		return runSwarmLeave([]string{"--team", "t"})
	})
	if err == nil {
		t.Fatal("expected error for missing --agent-id")
	}
}

func TestRunSwarmTasks_MissingTeam(t *testing.T) {
	_ = setupClaudeHome(t)
	_, err := captureStdout(t, func() error {
		return runSwarmTasks([]string{})
	})
	if err == nil {
		t.Fatal("expected error for missing --team")
	}
}

func TestRunSwarmTasks_InvalidStatus(t *testing.T) {
	home := setupClaudeHome(t)
	team := "st"
	writeTeamConfig(t, home, team, map[string]any{"name": team, "members": []any{}})
	writeTasksList(t, home, team, []map[string]any{
		{"id": "t1", "title": "One", "status": "pending"},
	})
	_, err := captureStdout(t, func() error {
		return runSwarmTasks([]string{"--team", team, "--status", "bogus"})
	})
	if err == nil {
		t.Fatal("expected error for invalid --status value")
	}
}

func TestRunSwarmClaim_MissingFlags(t *testing.T) {
	_ = setupClaudeHome(t)
	_, err := captureStdout(t, func() error {
		return runSwarmClaim([]string{"--me", "codex"})
	})
	if err == nil {
		t.Fatal("expected error for missing --team")
	}
	_, err = captureStdout(t, func() error {
		return runSwarmClaim([]string{"--team", "t", "--me", "codex"})
	})
	if err == nil {
		t.Fatal("expected error for missing --task")
	}
}

func TestRunSwarmComplete_MissingFlags(t *testing.T) {
	_ = setupClaudeHome(t)
	_, err := captureStdout(t, func() error {
		return runSwarmComplete([]string{"--me", "codex"})
	})
	if err == nil {
		t.Fatal("expected error for missing --team")
	}
	_, err = captureStdout(t, func() error {
		return runSwarmComplete([]string{"--team", "t", "--me", "codex"})
	})
	if err == nil {
		t.Fatal("expected error for missing --task")
	}
}

func TestRunSwarmBridge_MissingTeam(t *testing.T) {
	_ = setupClaudeHome(t)
	_, err := captureStdout(t, func() error {
		return runSwarmBridge([]string{"--me", "codex"})
	})
	if err == nil {
		t.Fatal("expected error for missing --team")
	}
}

func TestRunSwarmList_NonexistentTeamsDir(t *testing.T) {
	_ = setupClaudeHome(t)
	out, err := captureStdout(t, func() error {
		return runSwarmList([]string{"--json"})
	})
	if err != nil {
		t.Fatalf("runSwarmList should not error on missing teams dir: %v", err)
	}
	var teams []swarm.TeamSummary
	if err := json.Unmarshal([]byte(out), &teams); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(teams) != 0 {
		t.Fatalf("expected 0 teams, got %d", len(teams))
	}
}
