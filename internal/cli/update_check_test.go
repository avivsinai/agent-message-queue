package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/update"
)

func TestDrainJSONStdoutStaysParseableWithUpdateHint(t *testing.T) {
	root, _ := seedDrainMailbox(t)
	seedUpdateCache(t, "v0.73.0")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return Run([]string{"drain", "--root", root, "--me", "alice", "--json", "--include-body"}, "v0.70.0")
	})
	if err != nil {
		t.Fatalf("drain: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if strings.Contains(stdout, "update available") || strings.Contains(stdout, "9.9.9") {
		t.Fatalf("update hint leaked onto drain stdout:\n%s", stdout)
	}
	var result drainResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("drain stdout must stay machine-parseable: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if result.Count != 1 || len(result.Drained) != 1 || result.Drained[0].ID != "hint-msg" {
		t.Fatalf("drain payload = %#v, want the seeded message", result)
	}
	if !strings.Contains(stderr, "amq: update available (v0.70.0 -> v0.73.0)") {
		t.Fatalf("stderr missing update hint:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}

func TestDrainJSONDoesNotAdvertiseSentinelLatest(t *testing.T) {
	root, _ := seedDrainMailbox(t)
	seedUpdateCache(t, "v9.9.9")

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return Run([]string{"drain", "--root", root, "--me", "alice", "--json", "--include-body"}, "v0.70.0")
	})
	if err != nil {
		t.Fatalf("drain: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if strings.Contains(stdout, "9.9.9") || strings.Contains(stderr, "9.9.9") {
		t.Fatalf("sentinel leaked into drain streams:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	var result drainResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("drain stdout must stay machine-parseable: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if result.Count != 1 {
		t.Fatalf("drain payload = %#v, want the seeded message", result)
	}
}

func seedDrainMailbox(t *testing.T) (root, agent string) {
	t.Helper()
	root = t.TempDir()
	agent = "alice"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatal(err)
	}
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "hint-msg",
			From:    "bob",
			To:      []string{agent},
			Thread:  "p2p/alice__bob",
			Subject: "hint",
			Created: "2026-08-27T10:00:00Z",
		},
		Body: "body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, agent, "hint-msg.md", data); err != nil {
		t.Fatal(err)
	}
	return root, agent
}

func seedUpdateCache(t *testing.T, latest string) {
	t.Helper()
	cacheDir := t.TempDir()
	t.Setenv(update.EnvCacheDir, cacheDir)
	path, err := update.DefaultCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := update.SaveCache(path, &update.Cache{
		CheckedAt:     time.Now().UTC(),
		LatestVersion: latest,
	}); err != nil {
		t.Fatal(err)
	}
}
