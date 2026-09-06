package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCrossProjectSendFromOutsideTree(t *testing.T) {
	// End-to-end: runSend --project from outside the project tree,
	// single session, no AM_BASE_ROOT. This is the exact scenario
	// Codex flagged where session detection must use findAmqrcForRoot.
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")

	// Set up source project.
	srcProjectDir := filepath.Join(t.TempDir(), "src-project")
	srcSessionRoot := filepath.Join(srcProjectDir, ".agent-mail", "collab")
	if err := os.MkdirAll(srcSessionRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	// Set up peer project with matching session and agent inbox.
	peerProjectDir := filepath.Join(t.TempDir(), "peer-project")
	peerBaseRoot := filepath.Join(peerProjectDir, ".agent-mail")
	peerSessionRoot := filepath.Join(peerBaseRoot, "collab")
	for _, agent := range []string{"claude", "codex"} {
		if err := fsq.EnsureAgentDirs(peerSessionRoot, agent); err != nil {
			t.Fatal(err)
		}
		// outbox/sent for sender
		if err := os.MkdirAll(filepath.Join(srcSessionRoot, "agents", agent, "outbox", "sent"), 0o700); err != nil {
			t.Fatal(err)
		}
		// inbox dirs for sender (needed for validation)
		for _, sub := range []string{"tmp", "new", "cur"} {
			dir := filepath.Join(srcSessionRoot, "agents", agent, "inbox", sub)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Source .amqrc with peer mapping.
	srcRC := map[string]any{
		"root":    ".agent-mail",
		"project": "src-project",
		"peers": map[string]string{
			"peer-project": peerBaseRoot,
		},
	}
	rcData, _ := json.Marshal(srcRC)
	if err := os.WriteFile(filepath.Join(srcProjectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}

	// chdir OUTSIDE the project tree.
	outsideDir := t.TempDir()
	oldDir, _ := os.Getwd()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldDir) }()
	resetAmqrcCache()
	defer resetAmqrcCache()

	// Run send with --project, pointing --root at the session root.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSend([]string{
		"--me", "claude",
		"--root", srcSessionRoot,
		"--to", "codex",
		"--project", "peer-project",
		"--body", "hello from outside",
		"--json",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runSend: %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	var result map[string]any
	if err := json.Unmarshal(buf[:n], &result); err != nil {
		t.Fatalf("parse output: %v\nraw: %s", err, buf[:n])
	}

	// Verify delivery went to peer's collab session, not base root.
	deliveryRoot, _ := result["root"].(string)
	if !strings.HasSuffix(deliveryRoot, "/collab") {
		t.Errorf("expected delivery to collab session, got root=%q", deliveryRoot)
	}
	if result["cross_project"] != true {
		t.Error("expected cross_project=true in output")
	}

	// Verify message landed in peer's inbox.
	peerInbox := filepath.Join(peerSessionRoot, "agents", "codex", "inbox", "new")
	entries, _ := os.ReadDir(peerInbox)
	if len(entries) != 1 {
		t.Errorf("expected 1 message in peer inbox, got %d", len(entries))
	}
}
