package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

func TestThreadListIncludesRetainedHistoryAndHydrationMetadata(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}

	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	threadID := "work/reconcile"
	ids, _ := writeThreadOpenMessages(t, base, []threadOpenMessage{
		{name: "question", from: "alpha@s1", to: "beta@s1", kind: format.KindQuestion, thread: threadID},
		{name: "answer", from: "beta@s1", to: "alpha@s1", kind: format.KindAnswer, thread: threadID, refs: []string{"question"}},
		{name: "withdrawn", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, thread: threadID, labels: []string{"withdrawn"}},
		{name: "sent-only", from: "gamma@s2", to: "delta@s2", kind: format.KindStatus, thread: threadID, noInbox: true},
	}, func(i int, _ threadOpenMessage) time.Time {
		return time.Date(2026, time.January, 1, 0, 0, i+1, 0, time.UTC)
	})
	baseMessageID, err := format.NewMessageID(time.Date(2026, time.January, 1, 0, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	baseMessage, err := (format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: baseMessageID, From: "owner", To: []string{"reviewer"},
		Thread: threadID, Created: "2026-01-01T00:01:00Z", Refs: []string{"literal-ref"},
		ReplyTo: "codex@session1", ReplyProject: "amq-core", FromProject: "amq-board",
		Context: map[string]any{"path": "internal/board/store.go"},
	}, Body: "base message body\n"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(base, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(base, "reviewer"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(fsq.AgentOutboxSent(base, "owner"), baseMessageID+".md"),
		filepath.Join(fsq.AgentInboxNew(base, "reviewer"), baseMessageID+".md"),
	} {
		if err := os.WriteFile(path, baseMessage, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stdout, stderr := captureOutput(t, func() error {
		return runThread([]string{"list", "--all-sessions", "--json", "--root", filepath.Join(base, "s1")})
	})
	var result threadListResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if result.Schema != threadListSchema || result.BaseRoot != base || !slices.Equal(result.Sessions, []string{"", "s1", "s2"}) {
		t.Fatalf("result identity = schema %q, base %q, sessions %v", result.Schema, result.BaseRoot, result.Sessions)
	}
	if len(result.Threads) != 1 {
		t.Fatalf("threads = %+v, want one retained thread", result.Threads)
	}
	item := result.Threads[0]
	if item.ID != threadID || item.Count != 5 {
		t.Fatalf("thread = %+v, want %q with five messages", item, threadID)
	}
	slices.Sort(item.MessageIDs)
	wantIDs := []string{baseMessageID, ids["answer"], ids["question"], ids["sent-only"], ids["withdrawn"]}
	slices.Sort(wantIDs)
	if !slices.Equal(item.MessageIDs, wantIDs) {
		t.Fatalf("message_ids = %v, want %v", item.MessageIDs, wantIDs)
	}
	s1MessageIDs := []string{ids["answer"], ids["question"], ids["withdrawn"]}
	slices.Sort(s1MessageIDs)
	if len(item.Roots) != 3 || item.Roots[0].Session != "" || item.Roots[0].Root != base || !slices.Equal(item.Roots[0].Agents, []string{"owner", "reviewer"}) || !slices.Equal(item.Roots[0].MessageIDs, []string{baseMessageID}) || item.Roots[1].Session != "s1" || item.Roots[1].Root != filepath.Join(base, "s1") || !slices.Equal(item.Roots[1].Agents, []string{"alpha", "beta"}) || !slices.Equal(item.Roots[1].MessageIDs, s1MessageIDs) || item.Roots[2].Session != "s2" || item.Roots[2].Root != filepath.Join(base, "s2") || !slices.Equal(item.Roots[2].Agents, []string{"gamma"}) || !slices.Equal(item.Roots[2].MessageIDs, []string{ids["sent-only"]}) {
		t.Fatalf("hydration roots = %+v", item.Roots)
	}
	if !result.Coverage.Complete || result.Coverage.Messages != 5 || len(result.Coverage.Skipped) != 0 {
		t.Fatalf("coverage = %+v, want complete five-message scan", result.Coverage)
	}

	stdout, stderr = captureOutput(t, func() error {
		return runThread([]string{"--id", threadID, "--include-body", "--json", "--root", base, "--agents", "owner,reviewer"})
	})
	var hydrated []thread.Entry
	if err := json.Unmarshal([]byte(stdout), &hydrated); err != nil {
		t.Fatalf("unmarshal hydrated messages: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if len(hydrated) != 1 || hydrated[0].ID != baseMessageID || hydrated[0].Body != "base message body\n" || !slices.Equal(hydrated[0].Refs, []string{"literal-ref"}) || hydrated[0].ReplyTo != "codex@session1" || hydrated[0].ReplyProject != "amq-core" || hydrated[0].FromProject != "amq-board" || hydrated[0].Context["path"] != "internal/board/store.go" {
		t.Fatalf("hydrated message metadata = %+v", hydrated)
	}
}
