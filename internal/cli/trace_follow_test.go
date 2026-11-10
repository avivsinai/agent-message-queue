package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestTraceFollowFindsTheSessionThatDrainedACrossSessionSend(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "cto")
	targetRoot := filepath.Join(baseRoot, "qa")
	ensureSendSessionAgent(t, sourceRoot, "alice")
	ensureSendSessionAgent(t, targetRoot, "bob")
	sent := runSendJSONForTest(t, "--me", "alice", "--root", baseRoot, "--from-session", "cto",
		"--to", "bob", "--session", "qa", "--body", "hello", "--json")
	id := sent["id"].(string)
	if _, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", targetRoot, "--me", "bob", "--json"})
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}

	out, _, err := captureEnvOutput(t, func() error {
		return runTrace([]string{id, "--root", sourceRoot, "--follow", "--from", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("trace --follow: %v\n%s", err, out)
	}
	var result traceResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	follow := result.Follow
	if follow == nil || follow.Status != "found" || len(follow.Recipients) != 1 {
		t.Fatalf("follow = %s", out)
	}
	got := follow.Recipients[0]
	if got.Handle != "bob" || got.Session != "qa" || got.Delivery != "cur" || got.Receipt != "drained" {
		t.Fatalf("recipient = %+v, want bob in session qa, cur, drained", got)
	}
	expectSamePath(t, got.Root, targetRoot)
}

func TestThreadOpenFollowShowsThatAPeerProjectsReplyCameBack(t *testing.T) {
	tmp := t.TempDir()
	rootOf := func(project string) string { return filepath.Join(tmp, project, ".agent-mail") }
	ensureSendSessionAgent(t, rootOf("project-a"), "alice")
	ensureSendSessionAgent(t, rootOf("project-b"), "user")
	writeRouteAmqrc(t, filepath.Join(tmp, "project-a"), map[string]any{
		"root": ".agent-mail", "project": "project-a", "peers": map[string]string{"project-b": rootOf("project-b")},
	})
	writeRouteAmqrc(t, filepath.Join(tmp, "project-b"), map[string]any{
		"root": ".agent-mail", "project": "project-b", "peers": map[string]string{"project-a": rootOf("project-a")},
	})
	t.Setenv(envRoot, "")
	t.Setenv(envGlobalRoot, "")
	t.Chdir(tmp)
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	sent := runSendJSONForTest(t, "--me", "alice", "--root", rootOf("project-a"), "--to", "user",
		"--project", "project-b", "--kind", "question", "--body", "ship it?", "--json")
	id := sent["id"].(string)
	for _, run := range []func() error{
		func() error {
			return runReply([]string{"--root", rootOf("project-b"), "--me", "user", "--id", id, "--body", "yes"})
		},
		func() error { return runDrain([]string{"--root", rootOf("project-a"), "--me", "alice", "--json"}) },
	} {
		if out, _, err := captureEnvOutput(t, run); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}

	out, _, err := captureEnvOutput(t, func() error {
		return runThread([]string{"open", "--json", "--follow", "--root", rootOf("project-a")})
	})
	if err != nil {
		t.Fatalf("thread open --follow: %v\n%s", err, out)
	}
	var result threadOpenResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(result.Follow) != 1 || result.Follow[0].MessageID != id || len(result.Follow[0].Recipients) != 1 {
		t.Fatalf("follow = %s", out)
	}
	got := result.Follow[0].Recipients[0]
	if got.Handle != "user" || got.Project != "project-b" || got.Reply == nil || got.Reply.Status != "replied" ||
		len(got.Reply.Replies) != 1 || got.Reply.Replies[0].Delivery != "cur" || got.Reply.Replies[0].Receipt != "drained" {
		t.Fatalf("recipient = %s", out)
	}
	expectSamePath(t, got.Reply.Destination.Root, rootOf("project-a"))
}

// Review of trace --follow: a receipt file that does not parse counted as
// drained while the message was still new.
func TestTraceFollowDoesNotCountAnUnreadableReceiptAsDrained(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "cto")
	targetRoot := filepath.Join(baseRoot, "qa")
	ensureSendSessionAgent(t, sourceRoot, "alice")
	ensureSendSessionAgent(t, targetRoot, "bob")
	sent := runSendJSONForTest(t, "--me", "alice", "--root", baseRoot, "--from-session", "cto",
		"--to", "bob", "--session", "qa", "--body", "hello", "--json")
	id := sent["id"].(string)
	path := filepath.Join(fsq.AgentReceipts(targetRoot, "bob"), receipt.Filename(id, "bob", receipt.StageDrained))
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := runTraceFollowJSON(t, id, "--root", sourceRoot, "--from", "alice")
	if got := result.Follow.Recipients; len(got) != 1 || got[0].Delivery != "new" || got[0].Receipt != "unknown" {
		t.Fatalf("recipients = %+v, want bob new with receipt unknown", got)
	}
}

// Review of trace --follow: with a question to bob and carol and only bob's
// answer in alice's inbox, carol was reported as replied with bob's answer.
func TestTraceFollowCreditsEachRecipientOnlyWithItsOwnReply(t *testing.T) {
	root := setupReceiptsTestRoot(t)
	if err := fsq.EnsureAgentDirs(root, "carol"); err != nil {
		t.Fatal(err)
	}
	configureSendTestRoot(t, root, "alice", "bob", "carol")
	sent := runSendJSONForTest(t, "--root", root, "--me", "alice", "--to", "bob,carol", "--thread", "review/multi",
		"--kind", "question", "--body", "question", "--json")
	id := sent["id"].(string)
	if out, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{"--root", root, "--me", "bob", "--id", id, "--body", "yes", "--json"})
	}); err != nil {
		t.Fatalf("reply: %v\n%s", err, out)
	}

	result := runTraceFollowJSON(t, id, "--root", root, "--from", "alice")
	status := map[string]string{}
	for _, r := range result.Follow.Recipients {
		if r.Reply != nil {
			status[r.Handle] = r.Reply.Status
		}
	}
	if status["bob"] != "replied" || status["carol"] != "none" {
		t.Fatalf("reply status = %v, want bob replied and carol none", status)
	}
}

func runTraceFollowJSON(t *testing.T, id string, args ...string) traceResult {
	t.Helper()
	out, _, err := captureEnvOutput(t, func() error {
		return runTrace(append([]string{id, "--follow", "--json"}, args...))
	})
	if err != nil {
		t.Fatalf("trace --follow: %v\n%s", err, out)
	}
	var result traceResult
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Follow == nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	return result
}

// One recipient's unreadable inbox copy leaves the root readable for the
// next recipient (audit 2026-11-10).
func TestTraceFollowKeepsARecipientFailureToThatRecipient(t *testing.T) {
	root := setupReceiptsTestRoot(t)
	if err := fsq.EnsureAgentDirs(root, "carol"); err != nil {
		t.Fatal(err)
	}
	configureSendTestRoot(t, root, "alice", "bob", "carol")
	sent := runSendJSONForTest(t, "--root", root, "--me", "alice", "--to", "bob,carol", "--thread", "review/multi", "--body", "hello", "--json")
	id := sent["id"].(string)
	copyPath := filepath.Join(fsq.AgentInboxNew(root, "bob"), id+".md")
	if err := os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(copyPath, 0o700); err != nil {
		t.Fatal(err)
	}

	result := runTraceFollowJSON(t, id, "--root", root, "--from", "alice")
	got := map[string]followRecipient{}
	for _, r := range result.Follow.Recipients {
		got[r.Handle] = r
	}
	if got["bob"].Delivery != "unknown" || got["bob"].Detail == "" || got["carol"].Delivery != "new" {
		t.Fatalf("recipients = %+v, want bob unknown with detail and carol new", result.Follow.Recipients)
	}
}
