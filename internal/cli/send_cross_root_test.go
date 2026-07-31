package cli

import (
	"os"
	"path/filepath"
	"runtime"
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
	configureSendTestRoot(t, root, agents...)
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
	configureSendTestRoot(t, root, "claude", "bob")
	if err := runSend([]string{"--root", root, "--me", "claude", "--to", "bob", "--body", "hi"}); err != nil {
		t.Fatalf("bare-root send should succeed, got: %v", err)
	}
	if n := inboxCount(t, root, "bob"); n != 1 {
		t.Fatalf("expected 1 delivered message, got %d", n)
	}
}

func TestSend_RefusesUnroutedSameRootSelfAddress(t *testing.T) {
	root := sessionRoot(t, t.TempDir(), "collab", "claude")

	err := runSend([]string{
		"--root", root,
		"--me", "claude",
		"--to", "claude",
		"--body", "ambiguous self-send",
	})
	if err == nil {
		t.Fatal("expected self-send refusal, got nil")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "--allow-self") {
		t.Fatalf("error should name the explicit escape hatch, got: %v", err)
	}
	if n := inboxCount(t, root, "claude"); n != 0 {
		t.Fatalf("self-send refusal delivered %d message(s)", n)
	}
}

func TestSend_AllowSelfConfirmsUnroutedSameRootSelfAddress(t *testing.T) {
	root := sessionRoot(t, t.TempDir(), "collab", "claude")

	if err := runSend([]string{
		"--root", root,
		"--me", "claude",
		"--to", "claude",
		"--body", "intentional self-send",
		"--allow-self",
	}); err != nil {
		t.Fatalf("intentional self-send: %v", err)
	}
	header := soleDeliveredHeader(t, root, "claude")
	if header.From != "claude" || len(header.To) != 1 || header.To[0] != "claude" {
		t.Fatalf("unexpected self-send header: %+v", header)
	}
}

func TestSend_RefusesSelfRecipientBeforeMultiRecipientDelivery(t *testing.T) {
	root := sessionRoot(t, t.TempDir(), "collab", "claude", "codex")

	err := runSend([]string{
		"--root", root,
		"--me", "claude",
		"--to", "codex,claude",
		"--thread", "topic/ambiguous-self-send",
		"--body", "must not deliver partially",
	})
	if err == nil || !strings.Contains(err.Error(), "--allow-self") {
		t.Fatalf("expected self-recipient refusal, got: %v", err)
	}
	for _, agent := range []string{"claude", "codex"} {
		if n := inboxCount(t, root, agent); n != 0 {
			t.Fatalf("self-recipient refusal delivered %d message(s) to %s", n, agent)
		}
	}
}

func TestSend_AllowsRoutedSameHandleWithoutAllowSelf(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".agent-mail")
	sourceRoot := sessionRoot(t, tmp, "collab", "claude")
	targetRoot := sessionRoot(t, tmp, "auth", "claude")
	pinSendSessionForTest(t, base, sourceRoot, "collab")

	if err := runSend([]string{
		"--me", "claude",
		"--to", "claude",
		"--session", "auth",
		"--body", "routed same-handle message",
	}); err != nil {
		t.Fatalf("routed same-handle send: %v", err)
	}
	header := soleDeliveredHeader(t, targetRoot, "claude")
	if header.ReplyTo != "claude@collab" {
		t.Fatalf("reply_to = %q, want %q", header.ReplyTo, "claude@collab")
	}
}

func TestSend_RefusesRoutedSameHandleWhenTargetSessionIsSource(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".agent-mail")
	sourceRoot := sessionRoot(t, tmp, "collab", "claude")
	pinSendSessionForTest(t, base, sourceRoot, "collab")

	err := runSend([]string{
		"--me", "claude",
		"--to", "claude",
		"--session", "collab",
		"--body", "must not self-deliver through a redundant route",
	})
	assertPhysicalSelfSendRefused(t, err)
	if n := inboxCount(t, sourceRoot, "claude"); n != 0 {
		t.Fatalf("same-session route delivered %d message(s)", n)
	}
}

func TestSend_AllowSelfConfirmsRoutedSameHandleWhenTargetSessionIsSource(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".agent-mail")
	sourceRoot := sessionRoot(t, tmp, "collab", "claude")
	pinSendSessionForTest(t, base, sourceRoot, "collab")

	if err := runSend([]string{
		"--me", "claude",
		"--to", "claude",
		"--session", "collab",
		"--body", "intentional routed self-send",
		"--allow-self",
	}); err != nil {
		t.Fatalf("intentional routed self-send: %v", err)
	}
	if n := inboxCount(t, sourceRoot, "claude"); n != 1 {
		t.Fatalf("intentional routed self-send delivered %d message(s), want 1", n)
	}
}

func TestSend_RefusesSameSourceAndTargetSession(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".agent-mail")
	configureSendTestRoot(t, base, "claude")
	sourceRoot := filepath.Join(base, "collab")
	if err := fsq.EnsureAgentDirs(sourceRoot, "claude"); err != nil {
		t.Fatal(err)
	}

	err := runSend([]string{
		"--root", base,
		"--from-session", "collab",
		"--session", "collab",
		"--me", "claude",
		"--to", "claude",
		"--body", "must not self-deliver through matching sessions",
	})
	assertPhysicalSelfSendRefused(t, err)
	if n := inboxCount(t, sourceRoot, "claude"); n != 0 {
		t.Fatalf("matching source and target sessions delivered %d message(s)", n)
	}
}

func TestSend_RefusesSameRootPeerAlias(t *testing.T) {
	projectDir := t.TempDir()
	base := filepath.Join(projectDir, ".agent-mail")
	sourceRoot := sessionRoot(t, projectDir, "collab", "claude")
	writeRouteAmqrc(t, projectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"same-root": base,
		},
	})

	err := runSend([]string{
		"--root", sourceRoot,
		"--me", "claude",
		"--to", "claude",
		"--project", "same-root",
		"--body", "must not self-deliver through a peer alias",
	})
	assertPhysicalSelfSendRefused(t, err)
	if n := inboxCount(t, sourceRoot, "claude"); n != 0 {
		t.Fatalf("same-root peer alias delivered %d message(s)", n)
	}
}

func TestSend_RefusesSymlinkedSameRootPeerAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	projectDir := t.TempDir()
	base := filepath.Join(projectDir, ".agent-mail")
	sourceRoot := sessionRoot(t, projectDir, "collab", "claude")
	peerAlias := filepath.Join(t.TempDir(), "peer-root")
	if err := os.Symlink(base, peerAlias); err != nil {
		t.Fatal(err)
	}
	writeRouteAmqrc(t, projectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"same-root": peerAlias,
		},
	})

	err := runSend([]string{
		"--root", sourceRoot,
		"--me", "claude",
		"--to", "claude",
		"--project", "same-root",
		"--body", "must not self-deliver through a symlinked peer alias",
	})
	assertPhysicalSelfSendRefused(t, err)
	if n := inboxCount(t, sourceRoot, "claude"); n != 0 {
		t.Fatalf("symlinked same-root peer alias delivered %d message(s)", n)
	}
}

func TestSend_RefusesRoutedSameRootBeforeMultiRecipientDelivery(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, ".agent-mail")
	sourceRoot := sessionRoot(t, tmp, "collab", "claude", "codex")
	pinSendSessionForTest(t, base, sourceRoot, "collab")

	err := runSend([]string{
		"--me", "claude",
		"--to", "claude,codex",
		"--session", "collab",
		"--thread", "topic/routed-self-send",
		"--body", "must not deliver partially",
	})
	assertPhysicalSelfSendRefused(t, err)
	for _, agent := range []string{"claude", "codex"} {
		if n := inboxCount(t, sourceRoot, agent); n != 0 {
			t.Fatalf("routed self-recipient refusal delivered %d message(s) to %s", n, agent)
		}
	}
}

func assertPhysicalSelfSendRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected physical self-send refusal, got nil")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "--allow-self") {
		t.Fatalf("error should name the explicit escape hatch, got: %v", err)
	}
}

func TestSend_AllowSelfDoesNotBypassCrossTreeGuard(t *testing.T) {
	tmp := t.TempDir()
	sourceRoot := sessionRoot(t, filepath.Join(tmp, "projA"), "collab", "claude")
	targetRoot := sessionRoot(t, filepath.Join(tmp, "projB"), "collab", "claude")
	t.Setenv("AM_ROOT", sourceRoot)

	err := runSend([]string{
		"--root", targetRoot,
		"--me", "claude",
		"--to", "claude",
		"--body", "must stay local",
		"--allow-self",
	})
	if err == nil || !strings.Contains(err.Error(), "different AMQ tree") {
		t.Fatalf("--allow-self bypassed the cross-tree guard: %v", err)
	}
	if n := inboxCount(t, targetRoot, "claude"); n != 0 {
		t.Fatalf("cross-tree refusal delivered %d message(s)", n)
	}
}

// TestSend_AllowsRedundantSameTreeRoot: an explicit --root equal to (or within)
// the caller's own tree is not a crossing and must be allowed.
func TestSend_AllowsRedundantSameTreeRoot(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".claude", "agents"), 0o700); err != nil {
		t.Fatalf("mkdir unrelated agents dir: %v", err)
	}
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
