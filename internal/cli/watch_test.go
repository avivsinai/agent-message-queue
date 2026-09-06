package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunWatchExistingMessages(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message before watching
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-existing",
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "Existing message",
			Created: "2025-12-25T10:00:00Z",
		},
		Body: "This message exists before watch",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-existing.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err = runWatch([]string{"--root", root, "--me", "alice", "--json", "--timeout", "1s"})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runWatch: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result watchResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}

	if result.Event != "existing" {
		t.Errorf("expected event 'existing', got %s", result.Event)
	}
	if len(result.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(result.Messages))
	}
	if result.Messages[0].ID != "msg-existing" {
		t.Errorf("expected message ID 'msg-existing', got %s", result.Messages[0].ID)
	}
}

func TestRunWatchTimeout(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	start := time.Now()
	err := runWatch([]string{"--root", root, "--me", "alice", "--json", "--timeout", "100ms"})
	elapsed := time.Since(start)

	_ = w.Close()
	os.Stdout = oldStdout

	// Timeout now returns an error with ExitTimeout code
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if GetExitCode(err) != ExitTimeout {
		t.Errorf("expected ExitTimeout (%d), got %d", ExitTimeout, GetExitCode(err))
	}

	// Should timeout around 100ms
	if elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("expected timeout around 100ms, got %v", elapsed)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result watchResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (output: %s)", err, buf.String())
	}

	if result.Event != "timeout" {
		t.Errorf("expected event 'timeout', got %s", result.Event)
	}
}

func TestRunWatchRejectsReplacementRootWhileIdle(t *testing.T) {
	for _, poll := range []bool{true, false} {
		name := "fsnotify"
		if poll {
			name = "poll"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			active := filepath.Join(parent, "active")
			replacement := filepath.Join(parent, "replacement")
			displaced := filepath.Join(parent, "displaced")
			for _, root := range []string{active, replacement} {
				for _, agent := range []string{"alice", "bob"} {
					if err := fsq.EnsureAgentDirs(root, agent); err != nil {
						t.Fatalf("EnsureAgentDirs(%s,%s): %v", root, agent, err)
					}
				}
			}
			deliverGuardMessage(t, replacement, "alice", "replacement-message")

			ready := make(chan struct{})
			resume := make(chan struct{})
			oldIdleHook := watchIdleForTest
			watchIdleForTest = func() {
				close(ready)
				<-resume
			}
			t.Cleanup(func() { watchIdleForTest = oldIdleHook })

			oldStdout := os.Stdout
			stdoutReader, stdoutWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = stdoutWriter
			t.Cleanup(func() { os.Stdout = oldStdout })

			args := []string{
				"--root", active,
				"--me", "alice",
				"--json",
				"--timeout", "2s",
			}
			if poll {
				args = append(args, "--poll")
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runWatch(args) }()

			select {
			case <-ready:
			case watchErr := <-errCh:
				t.Fatalf("watch returned before idle swap: %v", watchErr)
			case <-time.After(2 * time.Second):
				t.Fatal("watch did not finish its initial empty scan")
			}
			if err := os.Rename(active, displaced); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, active); err != nil {
				t.Fatal(err)
			}
			close(resume)

			var watchErr error
			select {
			case watchErr = <-errCh:
			case <-time.After(3 * time.Second):
				t.Fatal("watch did not detect replacement root")
			}
			_ = stdoutWriter.Close()
			os.Stdout = oldStdout
			var stdout bytes.Buffer
			_, _ = stdout.ReadFrom(stdoutReader)

			if watchErr == nil || !strings.Contains(watchErr.Error(), "delivery root changed after authorization") {
				t.Fatalf("watch error = %v, want replacement-root refusal (stdout=%s)", watchErr, stdout.String())
			}
			if strings.Contains(stdout.String(), "replacement-message") {
				t.Fatalf("watch exposed replacement-root payload: %s", stdout.String())
			}
		})
	}
}
