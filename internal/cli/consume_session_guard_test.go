package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDrainRefusesSiblingSessionFromOverriddenAMRoot(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "sibling-theft")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runDrain([]string{"--me", "alice"})
	assertConsumeRefused(t, err, "drain")
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("foreign message count = %d, want 1 untouched in inbox/new", got)
	}
}

func TestDrainAllowsPinnedSessionRoot(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	root := sessionRoot(t, parent, "session1", "alice")
	deliverGuardMessage(t, root, "alice", "owned")

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	if err := runDrain([]string{"--me", "alice"}); err != nil {
		t.Fatalf("pinned session drain should succeed: %v", err)
	}
}

func TestDrainAllowsExplicitSessionRouting(t *testing.T) {
	parent := t.TempDir()
	authorizedParent := filepath.Join(parent, "authorized")
	authorizedBase := filepath.Join(authorizedParent, ".agent-mail")
	authorizedRoot := sessionRoot(t, authorizedParent, "session1", "alice")
	authorizedTarget := sessionRoot(t, authorizedParent, "session2", "alice")
	deliverGuardMessage(t, authorizedTarget, "alice", "authorized-target")

	t.Setenv("AM_ROOT", authorizedRoot)
	t.Setenv("AM_BASE_ROOT", authorizedBase)
	t.Setenv("AM_SESSION", "session1")

	if err := runDrain([]string{"--me", "alice", "--session", "session2"}); err != nil {
		t.Fatalf("explicit --session should route through the pinned base: %v", err)
	}
	if got := inboxCount(t, authorizedTarget, "alice"); got != 0 {
		t.Fatalf("authorized target count = %d, want 0 after routed drain", got)
	}
}

func TestEnvExportPinsAndClearsAMSession(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(project); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_SESSION", "temporary")
	if err := os.Unsetenv("AM_SESSION"); err != nil {
		t.Fatalf("unset AM_SESSION: %v", err)
	}
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "session1", "--me", "codex", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv session export: %v", err)
	}
	if !strings.Contains(stdout, "export AM_SESSION=session1\n") {
		t.Fatalf("session export must pin AM_SESSION, got %q", stdout)
	}

	baseRoot := filepath.Join(project, ".agent-mail")
	if err := os.MkdirAll(baseRoot, 0o700); err != nil {
		t.Fatalf("mkdir base root: %v", err)
	}
	t.Setenv("AM_BASE_ROOT", filepath.Join(project, "stale-base"))
	t.Setenv("AM_SESSION", "stale")
	stdout, _, err = captureEnvOutput(t, func() error {
		return runEnv([]string{"--root", baseRoot, "--me", "codex", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv base export: %v", err)
	}
	if !strings.Contains(stdout, "export AM_SESSION=\n") {
		t.Fatalf("sessionless export must clear stale AM_SESSION, got %q", stdout)
	}
}

func deliverGuardMessage(t *testing.T, root, agent, id string) {
	t.Helper()
	if err := deliverGuardMessageError(root, agent, id); err != nil {
		t.Fatalf("deliver message: %v", err)
	}
}

func deliverGuardMessageError(root, agent, id string) error {
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    "bob",
			To:      []string{agent},
			Thread:  "p2p/alice__bob",
			Subject: "guard test",
			Created: time.Now().UTC().Format(time.RFC3339Nano),
		},
		Body: "must remain owned by the target session",
	}
	data, err := msg.Marshal()
	if err != nil {
		return err
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	if _, err := fsq.DeliverToInbox(deliveryRoot, agent, id+".md", data); err != nil {
		return err
	}
	return nil
}

func assertConsumeRefused(t *testing.T, err error, command string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s refusal, got nil", command)
	}
	if code := GetExitCode(err); code != ExitContextMismatch {
		t.Fatalf("exit code = %d, want %d: %v", code, ExitContextMismatch, err)
	}
	if !strings.Contains(err.Error(), "refusing "+command) {
		t.Fatalf("error should explain %s refusal, got %v", command, err)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
