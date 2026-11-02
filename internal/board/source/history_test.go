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
}

// Wave1 review found that cwd/.beads split one backend into multiple task
// namespaces. The namespace belongs to bd's actual reported database.
func TestBeadsBindsReportedDatabaseAcrossWorktrees(t *testing.T) {
	backend, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(map[string]string{"database_path": backend})
	runner := captureRunner{"bd --readonly --sandbox info --json": info, "bd --readonly --sandbox list --json --all --limit 0": []byte(`[{"id":"demo-a1b","title":"Deliver outcome","status":"open"}]`)}
	source := Beads{IncludeClosed: true}
	for _, dir := range []string{"/main", "/worktree"} {
		result := source.Read(context.Background(), board.Env{Dir: dir, Now: time.Now, Run: runner})
		if result.Status != board.StatusOK || result.Namespace != backend || len(result.Items) != 1 {
			t.Fatalf("backend binding failed in %s: %+v", dir, result)
		}
	}
}
