package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudePrintLiveResumeAck(t *testing.T) {
	if os.Getenv("AMQ_CLAUDE_LIVE") != "1" {
		t.Skip("set AMQ_CLAUDE_LIVE=1 to resume a scratch Claude session")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, ".gitkeep"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	git := exec.Command("git", "init", "-q")
	git.Dir = scratch
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	uuid := liveScratchUUID(t)
	create := exec.Command(claudePath, "-p", "--session-id", uuid, "--permission-mode", "dontAsk",
		"Reply with exactly ACK and nothing else.")
	create.Dir = scratch
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create session: %v: %s", err, out)
	}
	payload := "AMQ_CLAUDE_LIVE ping"
	stateDir := t.TempDir()
	a := ClaudePrint{
		LookPath: func(string) (string, error) { return claudePath, nil },
		StateDir: stateDir,
		AckWait:  90 * time.Second,
	}
	target := claudePrintTargetSessionPrefix + uuid
	if err := a.Inject(context.Background(), target, payload); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "claude-print", uuid, "*.stream.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("stream logs = %v, %v, want 1", matches, err)
	}
	deadline := time.Now().Add(90 * time.Second)
	var sawAck, sawResult bool
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		sawAck = bytes.Contains(raw, []byte(`"isReplay":true`)) && bytes.Contains(raw, []byte(payload))
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var ev struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
			}
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			if ev.Type == "result" {
				sawResult = true
			}
		}
		if sawAck && sawResult {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !sawAck {
		t.Fatal("live log missing matching isReplay user event")
	}
	if !sawResult {
		t.Fatal("live log missing result event")
	}
	t.Logf("live log:\n%s", strings.TrimSpace(string(mustRead(t, matches[0]))))
}

func liveScratchUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
