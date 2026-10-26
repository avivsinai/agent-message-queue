package source

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

type captureRunner map[string][]byte

func (r captureRunner) Run(_ context.Context, _ string, argv []string) ([]byte, error) {
	out, ok := r[strings.Join(argv, " ")]
	if !ok {
		return nil, fmt.Errorf("unexpected command %v", argv)
	}
	return out, nil
}
func (r captureRunner) RunLimited(ctx context.Context, dir string, argv []string, _ int) ([]byte, error) {
	return r.Run(ctx, dir, argv)
}

// Wave1 review found that unioning IDs across roots hid a missing base copy
// when the same ID hydrated from a session. Each root owns its coverage.
func TestHistoryVerifiesEachRootAndNormalizesEvidence(t *testing.T) {
	metadata := `{"schema":"amq/thread-list/v1","generated_at":"first","base_root":"/base","threads":[{"thread_id":"work","message_ids":["ask"],"roots":[{"root":"/base","session":"","agents":["user"],"message_ids":["ask"]},{"root":"/base/session","session":"session","agents":["worker"],"message_ids":["ask"]}]}],"coverage":{"complete":true}}`
	body := `[{"id":"ask","thread":"work","from":"user","to":["worker"],"kind":"review_request","body":"Exact request","labels":["review"],"refs":["prior"],"reply_to":"user","reply_project":"peer","from_project":"origin","context":{"path":"file.go"}}]`
	runner := captureRunner{"amq thread list --all-sessions --json": []byte(metadata), "amq thread --id work --agents user --message-ids ask --include-body --json --root /base": []byte(`[]`), "amq thread --id work --agents worker --message-ids ask --include-body --json --root /base/session": []byte(body)}
	env := board.Env{Dir: "/project", Now: time.Now, Run: runner}
	result, messages, evidence := ReadHistory(context.Background(), env, "/base")
	if !result.Coverage.Partial() || !strings.Contains(result.Coverage.Detail, "/base: ask") || len(messages) != 1 {
		t.Fatalf("missing root copy hidden: %+v", result.Coverage)
	}
	message := messages[0]
	if message.Body != "Exact request" || len(message.Labels) != 1 || len(message.Refs) != 1 || message.ReplyTo != "user" || message.ReplyProject != "peer" || message.FromProject != "origin" || message.Context["path"] != "file.go" {
		t.Fatal("source metadata lost")
	}
	if len(evidence) != 1 || !json.Valid(evidence[0]) {
		t.Fatal("expected one immutable message evidence unit")
	}
	runner["amq thread list --all-sessions --json"] = []byte(strings.Replace(metadata, "first", "second", 1))
	_, again, nextEvidence := ReadHistory(context.Background(), env, "/base")
	if len(again) != 1 || again[0].EvidenceHash != message.EvidenceHash || len(nextEvidence) != 1 || string(nextEvidence[0]) != string(evidence[0]) {
		t.Fatal("acquisition timestamp changed evidence identity")
	}
	checkHistoryLogicalIdentity(t)
}

// Wave1 review found that cwd/.beads split one backend into multiple task
// namespaces. The namespace belongs to bd's actual reported database.
func TestBeadsBindsReportedDatabaseAcrossWorktrees(t *testing.T) {
	backend, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(map[string]string{"database_path": backend})
	runner := captureRunner{"bd --readonly --sandbox info --json": info, "bd --readonly --sandbox list --json --brief --all --limit 0": []byte(`[{"id":"demo-a1b","title":"Deliver outcome","status":"open"}]`)}
	source := Beads{IncludeClosed: true}
	for _, dir := range []string{"/main", "/worktree"} {
		result := source.Read(context.Background(), board.Env{Dir: dir, Now: time.Now, Run: runner})
		if result.Status != board.StatusOK || result.Namespace != backend || len(result.Items) != 1 {
			t.Fatalf("backend binding failed in %s: %+v", dir, result)
		}
	}
}

// Review found answered asks stayed unknown because an unrelated ambiguous
// follow-up invalidated the whole scope. Only native recipient outcomes close
// actions; missing facts and continuation-only IDs remain unknown.
func checkHistoryLogicalIdentity(t *testing.T) {
	t.Helper()
	metadata := `{"schema":"amq/thread-list/v1","base_root":"/base","threads":[{"thread_id":"work","message_ids":["ask"],"roots":[{"root":"/base","session":"","agents":["user","other"],"message_ids":["ask"]},{"root":"/base/session","session":"session","agents":["worker"],"message_ids":["ask"]}]}],"coverage":{"complete":true}}`
	body := `[{"id":"ask","thread":"work","from":"user","to":["worker"],"kind":"question","body":"Exact request"}]`
	coverage := `"coverage":{"max_age_seconds":0,"skipped":[],"unresolved_lineage":[],"ambiguous":[{"id":"unrelated-follow-up"}],"pruned_files":[],"expired":[],"outbound_routed":[{"session":"session","count":1}],"outbound_routed_openers":[{"id":"unrelated-outbound","session":"session"}],"skew_deferred":[],"truncated_by_limit":{"omitted":0}}`
	fold := func(outcomes string) []byte {
		return []byte(`{"schema":"amq/thread-open/v1","base_root":"/base","sessions":["","session"],` + strings.Replace(coverage, `"coverage":{`, `"coverage":{"action_outcomes":`+outcomes+`,`, 1) + `}`)
	}
	command := "amq thread open --all-sessions --include-base --max-age 0 --json"
	closed := `[{"opener_id":"ask","key":"session/ask/worker","state":"closed"}]`
	runner := captureRunner{"amq thread list --all-sessions --json": []byte(metadata), "amq thread --id work --agents user,other --message-ids ask --include-body --json --root /base": []byte(body), "amq thread --id work --agents worker --message-ids ask --include-body --json --root /base/session": []byte(body), command: fold(closed)}
	env := board.Env{Dir: "/project", Now: time.Now, Run: runner}
	result, messages, _ := ReadHistory(context.Background(), env, "/base")
	if len(messages) != 1 || messages[0].Key != "/base#ask" || len(messages[0].Copies) != 2 || len(messages[0].Copies[0].Agents) != 0 || result.ActionStates["/base#ask"] != "resolved" {
		t.Fatalf("logical identity, exact copy facts or lifecycle: %+v %+v", messages, result.ActionStates)
	}
	runner[command] = fold(`[{"opener_id":"ask","key":"session/ask/worker","state":"closed"},{"opener_id":"ask","key":"session/ask/other","state":"open"}]`)
	result, _, _ = ReadHistory(context.Background(), env, "/base")
	if result.ActionStates["/base#ask"] != "open" {
		t.Fatal("open recipient action hidden")
	}
	runner[command] = []byte(strings.Replace(string(fold(closed)), `"unrelated-outbound"`, `"ask"`, 1))
	result, _, _ = ReadHistory(context.Background(), env, "/base")
	if result.ActionStates["/base#ask"] != "unknown" {
		t.Fatal("unresolved routed recipient hidden by local closed action")
	}
	runner[command] = fold(`[]`)
	result, _, _ = ReadHistory(context.Background(), env, "/base")
	if result.ActionStates["/base#ask"] != "unknown" {
		t.Fatal("absence treated as resolution")
	}
	delete(runner, command)
	result, _, _ = ReadHistory(context.Background(), env, "/base")
	if result.ActionStates["/base#ask"] != "unknown" || !result.Coverage.Partial() {
		t.Fatal("failed lifecycle acquisition presented healthy")
	}
}
