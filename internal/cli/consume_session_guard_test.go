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

func TestDrainRefusesCrossTreeWithoutSessionPin(t *testing.T) {
	parent := t.TempDir()
	sourceBase := filepath.Join(parent, "source", ".agent-mail")
	targetRoot := sessionRoot(t, filepath.Join(parent, "target"), "session1", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "cross-tree-theft")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", sourceBase)
	t.Setenv("AM_SESSION", "temporary")
	if err := os.Unsetenv("AM_SESSION"); err != nil {
		t.Fatalf("unset AM_SESSION: %v", err)
	}

	err := runDrain([]string{"--me", "alice"})
	assertConsumeRefused(t, err, "drain")
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("foreign message count = %d, want 1 untouched in inbox/new", got)
	}
}

func TestDrainRefusesSameNamedSessionInDifferentBaseTree(t *testing.T) {
	parent := t.TempDir()
	sourceBase := filepath.Join(parent, "source", ".agent-mail")
	targetRoot := sessionRoot(t, filepath.Join(parent, "target"), "session1", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "same-name-foreign-tree")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", sourceBase)
	t.Setenv("AM_SESSION", "session1")

	err := runDrain([]string{"--me", "alice"})
	assertConsumeRefused(t, err, "drain")
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("foreign same-named session count = %d, want 1 untouched", got)
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
	ambientParent := filepath.Join(parent, "ambient")
	authorizedBase := filepath.Join(authorizedParent, ".agent-mail")
	_ = sessionRoot(t, authorizedParent, "session1", "alice")
	authorizedTarget := sessionRoot(t, authorizedParent, "session2", "alice")
	ambientRoot := sessionRoot(t, ambientParent, "session9", "alice")
	ambientTarget := sessionRoot(t, ambientParent, "session2", "alice")
	deliverGuardMessage(t, authorizedTarget, "alice", "authorized-target")
	deliverGuardMessage(t, ambientTarget, "alice", "ambient-target")

	t.Setenv("AM_ROOT", ambientRoot)
	t.Setenv("AM_BASE_ROOT", authorizedBase)
	t.Setenv("AM_SESSION", "session1")

	if err := runDrain([]string{"--me", "alice", "--session", "session2"}); err != nil {
		t.Fatalf("explicit --session should route through the pinned base: %v", err)
	}
	if got := inboxCount(t, authorizedTarget, "alice"); got != 0 {
		t.Fatalf("authorized target count = %d, want 0 after routed drain", got)
	}
	if got := inboxCount(t, ambientTarget, "alice"); got != 1 {
		t.Fatalf("ambient-base target count = %d, want 1 untouched", got)
	}
}

func TestDrainSessionRouteRequiresExistingMailbox(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	sourceRoot := sessionRoot(t, parent, "session1", "alice")
	missingMailboxRoot := filepath.Join(baseRoot, "session2")
	if err := os.MkdirAll(missingMailboxRoot, 0o700); err != nil {
		t.Fatalf("mkdir target root: %v", err)
	}

	t.Setenv("AM_ROOT", sourceRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runDrain([]string{"--me", "alice", "--session", "session2"})
	if err == nil || GetExitCode(err) != ExitNotFound {
		t.Fatalf("missing routed mailbox should be not-found, got %v", err)
	}
}

func TestDrainAllowsExplicitRootWhenIgnoringSessionPin(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	sourceRoot := sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "explicit-override")

	t.Setenv("AM_ROOT", sourceRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	if err := runDrain([]string{"--root", targetRoot, "--me", "alice", "--ignore-session-pin"}); err != nil {
		t.Fatalf("explicit root plus pin override should allow drain: %v", err)
	}
	if got := inboxCount(t, targetRoot, "alice"); got != 0 {
		t.Fatalf("target inbox/new count = %d, want 0 after override drain", got)
	}
}

func TestDrainRejectsPinOverrideWithoutExplicitRoot(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "ambient-override")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runDrain([]string{"--me", "alice", "--ignore-session-pin"})
	if err == nil || GetExitCode(err) != ExitUsage {
		t.Fatalf("ambient pin override should be a usage error, got %v", err)
	}
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("target count = %d, want 1 untouched", got)
	}
}

func TestDrainRejectsPinOverrideWithSessionRoute(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	sourceRoot := sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "routed-override")

	t.Setenv("AM_ROOT", sourceRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runDrain([]string{"--me", "alice", "--session", "session2", "--ignore-session-pin"})
	if err == nil || GetExitCode(err) != ExitUsage {
		t.Fatalf("pin override plus named route should be a usage error, got %v", err)
	}
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("target count = %d, want 1 untouched", got)
	}
}

func TestMonitorRefusesForeignSessionBeforeDrain(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "monitor-theft")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runMonitor([]string{"--me", "alice", "--timeout", "1ms"})
	assertConsumeRefused(t, err, "monitor")
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("foreign message count = %d, want 1 untouched in inbox/new", got)
	}
}

func TestMonitorMissingMailboxDoesNotCreateIt(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	root := filepath.Join(baseRoot, "session1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runMonitor([]string{"--me", "alice", "--timeout", "1ms", "--poll"})
	if err == nil || GetExitCode(err) != ExitNotFound {
		t.Fatalf("missing monitor mailbox should be not-found, got %v", err)
	}
	if _, statErr := os.Stat(fsq.AgentInboxNew(root, "alice")); !os.IsNotExist(statErr) {
		t.Fatalf("monitor created missing inbox, stat error = %v", statErr)
	}
}

func TestMonitorRechecksSessionPinBeforePostWatchDrain(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	root := sessionRoot(t, parent, "session1", "alice")

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	delivered := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := os.Setenv("AM_SESSION", "session2"); err != nil {
			delivered <- err
			return
		}
		delivered <- deliverGuardMessageError(root, "alice", "after-pin-change")
	}()

	err := runMonitor([]string{"--me", "alice", "--timeout", "2s", "--poll"})
	if deliverErr := <-delivered; deliverErr != nil {
		t.Fatalf("deliver message: %v", deliverErr)
	}
	assertConsumeRefused(t, err, "monitor")
	if got := inboxCount(t, root, "alice"); got != 1 {
		t.Fatalf("post-watch message count = %d, want 1 untouched", got)
	}
}

func TestMonitorMailboxDisappearanceIsNotTimeout(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	root := sessionRoot(t, parent, "session1", "alice")

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	removed := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		removed <- os.RemoveAll(fsq.AgentInboxNew(root, "alice"))
	}()

	err := runMonitor([]string{"--me", "alice", "--timeout", "2s", "--poll"})
	if removeErr := <-removed; removeErr != nil {
		t.Fatalf("remove inbox: %v", removeErr)
	}
	if err == nil || GetExitCode(err) != ExitNotFound {
		t.Fatalf("disappeared mailbox should be not-found, got %v", err)
	}
}

func TestMonitorMailboxDisappearanceWithFsnotifyIsNotTimeout(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	root := sessionRoot(t, parent, "session1", "alice")

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	removed := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		removed <- os.RemoveAll(fsq.AgentInboxNew(root, "alice"))
	}()

	err := runMonitor([]string{"--me", "alice", "--timeout", "2s"})
	if removeErr := <-removed; removeErr != nil {
		t.Fatalf("remove inbox: %v", removeErr)
	}
	if err == nil || GetExitCode(err) != ExitNotFound {
		t.Fatalf("fsnotify disappearance should be not-found, got %v", err)
	}
}

func TestReadRefusesForeignSessionBeforeMove(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "read-theft")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runRead([]string{"--me", "alice", "--id", "read-theft"})
	assertConsumeRefused(t, err, "read")
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("foreign message count = %d, want 1 untouched in inbox/new", got)
	}
}

func TestReadRefusesForeignSessionBeforeDLQMove(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "alice")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	badPath := filepath.Join(fsq.AgentInboxNew(targetRoot, "alice"), "corrupt.md")
	if err := os.WriteFile(badPath, []byte("not an AMQ message"), 0o600); err != nil {
		t.Fatalf("write corrupt message: %v", err)
	}

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")

	err := runRead([]string{"--me", "alice", "--id", "corrupt"})
	assertConsumeRefused(t, err, "read")
	if _, statErr := os.Stat(badPath); statErr != nil {
		t.Fatalf("corrupt message moved from inbox/new: %v", statErr)
	}
	dlqEntries, dlqErr := os.ReadDir(fsq.AgentDLQNew(targetRoot, "alice"))
	if dlqErr != nil {
		t.Fatalf("read DLQ: %v", dlqErr)
	}
	if len(dlqEntries) != 0 {
		t.Fatalf("foreign read moved %d message(s) to DLQ", len(dlqEntries))
	}
}

func TestDrainAllowsBareRootWithoutSessionIdentity(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	deliverGuardMessage(t, root, "alice", "bare-root")

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_SESSION", "temporary")
	if err := os.Unsetenv("AM_SESSION"); err != nil {
		t.Fatalf("unset AM_SESSION: %v", err)
	}

	if err := runDrain([]string{"--root", root, "--me", "alice"}); err != nil {
		t.Fatalf("bare-root drain should stay fail-open: %v", err)
	}
}

func TestDrainTreatsPresentEmptySessionPinAsBaseContext(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	targetRoot := sessionRoot(t, parent, "session2", "alice")
	deliverGuardMessage(t, targetRoot, "alice", "base-pin-mismatch")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "")

	err := runDrain([]string{"--me", "alice"})
	assertConsumeRefused(t, err, "drain")
	if got := inboxCount(t, targetRoot, "alice"); got != 1 {
		t.Fatalf("target count = %d, want 1 untouched", got)
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

func TestEnvRejectsAmbientRootLaundering(t *testing.T) {
	parent := t.TempDir()
	baseRoot := filepath.Join(parent, ".agent-mail")
	_ = sessionRoot(t, parent, "session1", "codex")
	targetRoot := sessionRoot(t, parent, "session2", "codex")

	t.Setenv("AM_ROOT", targetRoot)
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")
	t.Setenv("AM_ME", "codex")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv(nil)
	})
	if err == nil || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("plain env should reject laundering a mismatched AM_ROOT, got %v", err)
	}
	if stdout != "" {
		t.Fatalf("mismatched env emitted shell commands: %q", stdout)
	}
}

func TestEnvExplicitRepinReplacesFullContext(t *testing.T) {
	project := t.TempDir()
	baseRoot := filepath.Join(project, ".agent-mail")
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}
	_ = sessionRoot(t, project, "session1", "codex")
	_ = sessionRoot(t, project, "session2", "codex")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(project); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	resolvedProject, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolved getwd: %v", err)
	}
	baseRoot = filepath.Join(resolvedProject, ".agent-mail")

	t.Setenv("AM_ROOT", filepath.Join(baseRoot, "session1"))
	t.Setenv("AM_BASE_ROOT", baseRoot)
	t.Setenv("AM_SESSION", "session1")
	t.Setenv("AM_ME", "codex")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "session2", "--me", "codex"})
	})
	if err != nil {
		t.Fatalf("explicit session repin: %v", err)
	}
	for _, want := range []string{
		"export AM_ROOT=" + filepath.Join(baseRoot, "session2") + "\n",
		"export AM_BASE_ROOT=" + baseRoot + "\n",
		"export AM_SESSION=session2\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("session repin missing %q: %q", want, stdout)
		}
	}

	stdout, _, err = captureEnvOutput(t, func() error {
		return runEnv([]string{"--root", baseRoot, "--me", "codex"})
	})
	if err != nil {
		t.Fatalf("explicit base repin: %v", err)
	}
	if !strings.Contains(stdout, "export AM_BASE_ROOT="+baseRoot+"\n") || !strings.Contains(stdout, "export AM_SESSION=\n") {
		t.Fatalf("base repin did not replace session context: %q", stdout)
	}

	stdout, _, err = captureEnvOutput(t, func() error {
		return runEnv([]string{"--root", baseRoot, "--me", "codex", "--shell", "fish"})
	})
	if err != nil {
		t.Fatalf("explicit fish base repin: %v", err)
	}
	if !strings.Contains(stdout, "set -gx AM_BASE_ROOT "+baseRoot+"\n") || !strings.Contains(stdout, "set -gx AM_SESSION ''\n") {
		t.Fatalf("fish base repin did not replace session context: %q", stdout)
	}
}

func TestBuildCoopExecEnvironmentPinsSessionIdentity(t *testing.T) {
	base := []string{
		"PATH=/bin",
		"AM_ROOT=/stale/root",
		"AM_BASE_ROOT=/stale/base",
		"AM_SESSION=stale",
	}
	root := filepath.Join(t.TempDir(), ".agent-mail", "session1")
	env := buildCoopExecEnvironment(base, root, "codex", "session1")

	if got := envValue(env, "AM_ROOT"); got != root {
		t.Fatalf("AM_ROOT = %q, want %q", got, root)
	}
	if got := envValue(env, "AM_BASE_ROOT"); got != filepath.Dir(root) {
		t.Fatalf("AM_BASE_ROOT = %q, want %q", got, filepath.Dir(root))
	}
	if got := envValue(env, "AM_SESSION"); got != "session1" {
		t.Fatalf("AM_SESSION = %q, want session1", got)
	}
	if got := envValue(env, "AM_ME"); got != "codex" {
		t.Fatalf("AM_ME = %q, want codex", got)
	}

	customRoot := filepath.Join(t.TempDir(), "custom-root")
	env = buildCoopExecEnvironment(base, customRoot, "codex", "")
	if got := envValue(env, "AM_SESSION"); got != "" {
		t.Fatalf("custom sessionless --root should clear AM_SESSION, got %q", got)
	}
	if !envHasKey(env, "AM_BASE_ROOT") {
		t.Fatalf("custom sessionless --root should include AM_BASE_ROOT: %#v", env)
	}
	if got := envValue(env, "AM_BASE_ROOT"); got != customRoot {
		t.Fatalf("custom sessionless --root should pin exact AM_BASE_ROOT, got %q", got)
	}
}

func TestCoopSessionIdentityDistinguishesNamedAndSessionlessRoots(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".agent-mail")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	if got := coopSessionIdentity(filepath.Join(base, "auth"), "auth", filepath.Join(base, "auth")); got != "auth" {
		t.Fatalf("explicit --session identity = %q, want auth", got)
	}
	if got := coopSessionIdentity(filepath.Join(base, "collab"), "", ""); got != defaultSessionName {
		t.Fatalf("default identity = %q, want %q", got, defaultSessionName)
	}
	if got := coopSessionIdentity(filepath.Join(base, "auth"), "", filepath.Join(base, "auth")); got != "auth" {
		t.Fatalf("session-shaped explicit --root identity = %q, want auth", got)
	}
	if got := coopSessionIdentity(filepath.Join(t.TempDir(), "custom-root"), "", "/custom-root"); got != "" {
		t.Fatalf("custom sessionless --root identity = %q, want empty", got)
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
	if _, err := fsq.DeliverToInbox(root, agent, id+".md", data); err != nil {
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

func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
