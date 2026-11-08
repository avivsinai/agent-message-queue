package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"
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
