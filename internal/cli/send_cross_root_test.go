package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// sessionRoot builds <parent>/.agent-mail/<session> with the given agents and
// returns the session root. The ".agent-mail" parent makes classifyRoot treat it
// as a session under a base, so senderInSession / cross-tree detection behave as
// they do in a real coop layout.
func sessionRoot(t *testing.T, parent, session string, agents ...string) string {
	t.Helper()
	root := filepath.Join(parent, ".agent-mail", session)
	for _, a := range agents {
		if err := fsq.EnsureAgentDirs(root, a); err != nil {
			t.Fatalf("EnsureAgentDirs(%s, %s): %v", root, a, err)
		}
	}
	return root
}

func inboxCount(t *testing.T, root, agent string) int {
	t.Helper()
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, agent))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("ReadDir inbox: %v", err)
	}
	return len(entries)
}

// soleDeliveredHeader returns the header of the single message delivered to
// agent's inbox/new, failing if there is not exactly one.
func soleDeliveredHeader(t *testing.T, root, agent string) format.Header {
	t.Helper()
	dir := fsq.AgentInboxNew(root, agent)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 delivered message for %s, got %d", agent, len(entries))
	}
	hdr, err := format.ReadHeaderFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadHeaderFile: %v", err)
	}
	return hdr
}

// TestSend_RefusesUnqualifiedCrossTreeRoot is the core #144 guard: a direct
// --root into a different base tree, with no routing dimension, must fail loudly
// and deliver nothing — rather than mint an unreplyable message.
func TestSend_RefusesUnqualifiedCrossTreeRoot(t *testing.T) {
	tmp := t.TempDir()
	srcRoot := sessionRoot(t, filepath.Join(tmp, "projA"), "collab", "claude")
	dstRoot := sessionRoot(t, filepath.Join(tmp, "projB"), "collab", "claude")

	t.Run("AM_ROOT evidence", func(t *testing.T) {
		t.Setenv("AM_ROOT", srcRoot)
		err := runSend([]string{"--root", dstRoot, "--me", "claude", "--to", "claude", "--body", "evidence"})
		if err == nil {
			t.Fatal("expected refusal, got nil")
		}
		if code := GetExitCode(err); code != ExitUsage {
			t.Fatalf("exit code = %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(err.Error(), "refusing send") {
			t.Errorf("error should explain the refusal, got: %v", err)
		}
		if n := inboxCount(t, dstRoot, "claude"); n != 0 {
			t.Fatalf("nothing should be delivered, got %d", n)
		}
	})

	t.Run("AM_BASE_ROOT evidence", func(t *testing.T) {
		t.Setenv("AM_BASE_ROOT", filepath.Join(tmp, "projA", ".agent-mail"))
		err := runSend([]string{"--root", dstRoot, "--me", "claude", "--to", "claude", "--body", "evidence"})
		if err == nil || !strings.Contains(err.Error(), "refusing send") {
			t.Fatalf("expected refusal via AM_BASE_ROOT, got: %v", err)
		}
		if n := inboxCount(t, dstRoot, "claude"); n != 0 {
			t.Fatalf("nothing should be delivered, got %d", n)
		}
	})
}

// TestSend_AllowsBareRootWithoutIdentity guards the "no evidence ⇒ allow"
// invariant: with no AM_ROOT / AM_BASE_ROOT / project .amqrc (the CI/test/script
// case), an explicit --root to a temp dir must still deliver.
func TestSend_AllowsBareRootWithoutIdentity(t *testing.T) {
	root := t.TempDir()
	for _, a := range []string{"claude", "bob"} {
		if err := fsq.EnsureAgentDirs(root, a); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}
	if err := runSend([]string{"--root", root, "--me", "claude", "--to", "bob", "--body", "hi"}); err != nil {
		t.Fatalf("bare-root send should succeed, got: %v", err)
	}
	if n := inboxCount(t, root, "bob"); n != 1 {
		t.Fatalf("expected 1 delivered message, got %d", n)
	}
}

// TestSend_AllowsRedundantSameTreeRoot: an explicit --root equal to (or within)
// the caller's own tree is not a crossing and must be allowed.
func TestSend_AllowsRedundantSameTreeRoot(t *testing.T) {
	tmp := t.TempDir()
	root := sessionRoot(t, tmp, "collab", "claude", "codex")
	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", filepath.Join(tmp, ".agent-mail"))
	if err := runSend([]string{"--root", root, "--me", "claude", "--to", "codex", "--body", "hi"}); err != nil {
		t.Fatalf("same-tree --root should succeed, got: %v", err)
	}
	if n := inboxCount(t, root, "codex"); n != 1 {
		t.Fatalf("expected 1 delivered message, got %d", n)
	}
}

// TestSend_SameSessionOmitsReplyTo verifies point 2: an ordinary same-session
// send no longer stamps reply_to (which is what made cross-root sends look
// replyable while looping locally).
func TestSend_SameSessionOmitsReplyTo(t *testing.T) {
	tmp := t.TempDir()
	root := sessionRoot(t, tmp, "collab", "claude", "codex")
	if err := runSend([]string{"--root", root, "--me", "claude", "--to", "codex", "--body", "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	hdr := soleDeliveredHeader(t, root, "codex")
	if hdr.ReplyTo != "" {
		t.Errorf("same-session reply_to should be empty, got %q", hdr.ReplyTo)
	}
	if hdr.ReplyProject != "" {
		t.Errorf("same-session reply_project should be empty, got %q", hdr.ReplyProject)
	}
}

// TestSend_CrossSessionStampsReplyTo verifies point 2 did not over-remove:
// a real cross-session send (--session) still stamps reply_to for routing back.
func TestSend_CrossSessionStampsReplyTo(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".agent-mail")
	srcRoot := sessionRoot(t, tmp, "collab", "claude")
	dstRoot := sessionRoot(t, tmp, "auth", "codex")
	_ = srcRoot

	t.Setenv("AM_ROOT", srcRoot)
	t.Setenv("AM_BASE_ROOT", base)
	if err := runSend([]string{"--me", "claude", "--to", "codex", "--session", "auth", "--body", "hi"}); err != nil {
		t.Fatalf("cross-session send: %v", err)
	}
	hdr := soleDeliveredHeader(t, dstRoot, "codex")
	if hdr.ReplyTo != "claude@collab" {
		t.Errorf("cross-session reply_to = %q, want %q", hdr.ReplyTo, "claude@collab")
	}
}
