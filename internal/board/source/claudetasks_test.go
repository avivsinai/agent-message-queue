package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

// amq-board showed "Claude task producer version unsupported" and no tasks
// for the installed Claude Code 2.1.312 (2026-10-29): a version list gated a
// payload whose shape was unchanged. This unit fixture exercises schema and
// project/list bindings, not live native task acceptance.
func TestClaudeNativeTasksUseHomeListAndTaskIdentity(t *testing.T) {
	version := "2.1.312"
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := "019f0000-0000-7000-8000-000000000001"
	for _, dir := range []string{filepath.Join(home, "tasks", session), filepath.Join(home, "tasks", "custom-team"), filepath.Join(home, "projects", "project")} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	payload := []byte(`{"id":"1","subject":"Deliver native outcome","description":"Native task description","activeForm":"Delivering outcome","owner":"native-worker","status":"in_progress","blocks":[],"blockedBy":[],"metadata":{"origin":"native"}}`)
	for _, list := range []string{session, "custom-team"} {
		if err = os.WriteFile(filepath.Join(home, "tasks", list, "1.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	header, _ := json.Marshal(map[string]string{"sessionId": session, "cwd": project, "version": version})
	if err = os.WriteFile(filepath.Join(home, "projects", "project", session+".jsonl"), append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := captureRunner{"claude --version": []byte(version + " (Claude Code)\n"), "git worktree list --porcelain -z": []byte("worktree " + project + "\x00\x00"), "amq session list --json": []byte(`{"sessions":[{"name":"s1","conversations":[{"provider":"claude","state":"ready","id":"` + session + `","handle":"host-agent"}]}]}`)}
	env := board.Env{Dir: project, Now: time.Now, Run: runner}
	result := (ClaudeTasks{Home: home}).Read(context.Background(), env)
	if len(result.Items) != 1 || result.Items[0].Namespace != board.ClaudeListKey(home, session) || result.Items[0].Ref.ID != "1" || result.Items[0].NativeOwner != "native-worker" || result.Items[0].Owner.Kind != board.OwnerUnmapped || result.Items[0].HostAttribution == nil || result.Items[0].HostAttribution.Handle != "host-agent" {
		t.Fatalf("default native binding: %+v", result.Items)
	}
	if result.Version != "Claude Code "+version+" filesystem" {
		t.Fatalf("producer version: %q", result.Version)
	}
	key := board.ClaudeListKey(home, "custom-team")
	binding := board.ClaudeListBinding{Home: home, List: "custom-team", Revision: "owner-grant"}
	result = (ClaudeTasks{Home: home, Bindings: map[string]board.ClaudeListBinding{key: binding}}).Read(context.Background(), env)
	if len(result.Items) != 2 || !result.NativeScopes[key].Complete || !result.Coverage.Partial() {
		t.Fatalf("custom binding or backend coverage: %+v", result)
	}
	for _, item := range result.Items {
		if item.Namespace == key && (item.BindingRevision != "owner-grant" || item.HostAttribution != nil) {
			t.Fatal("custom native list borrowed a session attribution")
		}
	}
	checkClaudeAggregateBudget(t, env)
}

// amq-board showed claude-tasks degraded on the owner's Mac (2026-10-29):
// 106 native task lists under ~/.claude/tasks held no task file, and each
// was reported as an unbound list. An empty list hides no task.
func TestClaudeNativeTasksSkipAnEmptyUnboundList(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(home, "tasks", "session-00d9ec66"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := captureRunner{"claude --version": []byte("2.1.312 (Claude Code)\n"), "git worktree list --porcelain -z": []byte("worktree " + home + "\x00\x00"), "amq session list --json": []byte(`{"sessions":[]}`)}
	env := board.Env{Dir: home, Now: time.Now, Run: runner}
	result := (ClaudeTasks{Home: home}).Read(context.Background(), env)
	if strings.Contains(result.Coverage.Detail, "unbound native task list") {
		t.Fatalf("coverage = %q; an empty list is not a gap", result.Coverage.Detail)
	}
}

// Native review observed the per-file cap still allowed 10 GiB per home.
// Two real private file scopes must share one aggregate acquisition budget.
func checkClaudeAggregateBudget(t *testing.T, env board.Env) {
	t.Helper()
	var homes []string
	bindings := map[string]board.ClaudeListBinding{}
	for h := 0; h < 2; h++ {
		home, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		homes = append(homes, home)
		list := "budget-list"
		dir := filepath.Join(home, "tasks", list)
		if err = os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		key := board.ClaudeListKey(home, list)
		bindings[key] = board.ClaudeListBinding{Home: home, List: list, Revision: fmt.Sprintf("grant-%d", h)}
		for i := 1; i <= 9; i++ {
			payload, _ := json.Marshal(map[string]any{"id": fmt.Sprint(i), "subject": "Native outcome", "description": strings.Repeat("x", (1<<20)-1024), "status": "pending", "blocks": []string{}, "blockedBy": []string{}})
			if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", i)), payload, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	result := (ClaudeTasks{Home: homes[0], Bindings: bindings}).Read(context.Background(), env)
	if len(result.Items) >= 18 || !result.Coverage.Partial() || !strings.Contains(result.Coverage.Detail, "aggregate native acquisition byte budget reached") {
		t.Fatal("native homes did not share aggregate byte budget")
	}
	if !result.NativeScopes[board.ClaudeListKey(homes[0], "budget-list")].Complete || result.NativeScopes[board.ClaudeListKey(homes[1], "budget-list")].Complete {
		t.Fatal("acquisition bound did not preserve per-scope completeness")
	}
}
