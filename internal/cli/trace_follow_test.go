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
