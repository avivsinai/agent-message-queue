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

func TestRouteExplainSameRootConfiguredMissingMailboxIsRoutableWithoutRepair(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	missing := filepath.Join(root, "agents", "bob")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}

	result := runRouteExplainJSONForTest(t,
		"--from-root", root,
		"--me", "alice",
		"--to", "bob",
	)
	if !result.Routable {
		t.Fatalf("configured local route = non-routable: %s", result.Error)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("route explain repaired destination mailbox: %v", err)
	}

	runExplainedSend(t, result, "same-root repair")
	assertCompleteSendMailbox(t, root, "bob")
	if got := inboxCount(t, root, "bob"); got != 1 {
		t.Fatalf("delivered messages = %d, want 1", got)
	}
}

func TestRouteExplainMissingUnconfiguredMailboxMatchesDefaultSendAndPreservesStrictRefusal(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice")
	missing := filepath.Join(root, "agents", "bob")

	_, _, strictErr := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "alice",
			"--to", "bob",
			"--strict",
			"--body", "must reject",
		})
	})
	if strictErr == nil || !strings.Contains(strictErr.Error(), `handle "bob" not in config.json`) {
		t.Fatalf("strict send error = %v, want unknown-handle refusal", strictErr)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("strict send changed missing destination mailbox: %v", err)
	}

	result := runRouteExplainJSONForTest(t,
		"--from-root", root,
		"--me", "alice",
		"--to", "bob",
	)
	if !result.Routable {
		t.Fatalf("default non-strict send route = non-routable: %s", result.Error)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("route explain repaired unconfigured destination mailbox: %v", err)
	}

	args := append([]string(nil), result.Argv[2:]...)
	args = append(args, "--body", "default non-strict repair", "--json")
	_, stderr, err := captureEnvOutput(t, func() error {
		return runSend(args)
	})
	if err != nil {
		t.Fatalf("explained default send: %v", err)
	}
	if !strings.Contains(stderr, `warning: handle "bob" not in config.json`) {
		t.Fatalf("default send omitted unknown-handle warning: %q", stderr)
	}
	assertCompleteSendMailbox(t, root, "bob")
	if got := inboxCount(t, root, "bob"); got != 1 {
		t.Fatalf("delivered messages = %d, want 1", got)
	}
}

func TestRouteExplainAndGeneratedSendRejectContradictoryLegacyPin(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, ".agent-mail")
	source := sessionRoot(t, parent, "dev", "alice")
	target := sessionRoot(t, parent, "qa", "bob")
	foreign := sessionRoot(t, t.TempDir(), "foreign", "alice")

	t.Setenv(envRoot, foreign)
	t.Setenv(envBaseRoot, base)
	t.Setenv(envSession, "dev")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	result := runRouteExplainJSONForTest(t,
		"--from-root", source,
		"--me", "alice",
		"--to", "bob",
		"--session", "qa",
	)
	if result.Routable || !strings.Contains(result.Error, "differs from pinned root") {
		t.Fatalf("route explain result = %#v, want legacy AM_ROOT/pin mismatch", result)
	}
	if len(result.Argv) != 0 {
		t.Fatalf("unroutable argv = %v, want empty", result.Argv)
	}

	generated := buildRouteArgv(source, "alice", "bob", "", "qa")
	sendArgs := append([]string(nil), generated[2:]...)
	sendArgs = append(sendArgs, "--body", "must not deliver")
	_, _, err := captureEnvOutput(t, func() error {
		return runSend(sendArgs)
	})
	if err == nil || GetExitCode(err) != ExitContextMismatch ||
		!strings.Contains(err.Error(), "differs from pinned root") {
		t.Fatalf("generated send error = %v, want matching context mismatch", err)
	}
	if got := inboxCount(t, target, "bob"); got != 0 {
		t.Fatalf("contradictory legacy pin delivered %d message(s), want 0", got)
	}
}

func TestRouteExplainRejectsCompleteUnconfiguredLocalMailboxLikeSend(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := t.TempDir()
	ensureRouteAgents(t, root, "alice", "bob")

	result := runRouteExplainJSONForTest(t,
		"--from-root", root,
		"--me", "alice",
		"--to", "bob",
	)
	if result.Routable {
		t.Fatalf("unconfigured route was advertised as routable: %#v", result)
	}
	if !strings.Contains(result.Error, "not initialized") ||
		!strings.Contains(result.Error, "meta/config.json") {
		t.Fatalf("route error = %q, want send-compatible uninitialized error", result.Error)
	}
	if len(result.Argv) != 0 {
		t.Fatalf("unroutable argv = %v, want empty", result.Argv)
	}

	_, _, sendErr := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "alice",
			"--to", "bob",
			"--body", "must reject",
		})
	})
	if GetExitCode(sendErr) != ExitNotFound {
		t.Fatalf("send error = %v (exit %d), want not-found", sendErr, GetExitCode(sendErr))
	}
	if !strings.Contains(sendErr.Error(), "not initialized") ||
		!strings.Contains(sendErr.Error(), "meta/config.json") {
		t.Fatalf("send error = %v, want matching uninitialized cause", sendErr)
	}
}

func TestRouteExplainCrossSessionConfiguredMissingMailboxIsRoutableWithoutRepair(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	source := filepath.Join(base, "dev")
	target := filepath.Join(base, "qa")
	configureSendTestRoot(t, source, "alice")
	configureSendTestRoot(t, target, "bob")
	if err := fsq.EnsureAgentDirs(source, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(target, "bob"); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(target, "agents", "bob")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}

	result := runRouteExplainJSONForTest(t,
		"--from-root", source,
		"--me", "alice",
		"--to", "bob",
		"--session", "qa",
	)
	if !result.Routable {
		t.Fatalf("configured cross-session route = non-routable: %s", result.Error)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("route explain repaired destination mailbox: %v", err)
	}

	runExplainedSend(t, result, "cross-session repair")
	assertCompleteSendMailbox(t, target, "bob")
	if got := inboxCount(t, target, "bob"); got != 1 {
		t.Fatalf("delivered messages = %d, want 1", got)
	}
}

func TestRouteExplainBaseOnlyCoopConfigTreatsMissingLocalLeafAsRepairable(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	source := filepath.Join(base, "collab")
	target := filepath.Join(base, "qa")
	configureSendTestRoot(t, base, "alice", "bob")
	if err := fsq.EnsureAgentDirs(source, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(target, "bob"); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(target, "agents", "bob")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	pinSendSessionForTest(t, base, source, "collab")

	result := runRouteExplainJSONForTest(t,
		"--from-root", source,
		"--me", "alice",
		"--to", "bob",
		"--session", "qa",
	)
	if !result.Routable {
		t.Fatalf("base-authorized local route = non-routable: %s", result.Error)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("route explain repaired base-authorized leaf: %v", err)
	}

	runExplainedSend(t, result, "base-authorized repair")
	assertCompleteSendMailbox(t, target, "bob")
	if _, err := os.Lstat(filepath.Join(target, "meta", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("send copied base config into target session: %v", err)
	}
}

func TestRouteExplainExplicitUnpinnedSessionsUseBaseOnlyCoopConfig(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	source := filepath.Join(base, "dev")
	target := filepath.Join(base, "qa")
	configureSendTestRoot(t, base, "alice", "bob")
	configPath := filepath.Join(base, "meta", "config.json")
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(source, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureRootDirs(target); err != nil {
		t.Fatal(err)
	}

	result := runRouteExplainJSONForTest(t,
		"--from-root", source,
		"--me", "alice",
		"--to", "bob",
		"--session", "qa",
	)
	if !result.Routable {
		t.Fatalf("base-authorized unpinned route = non-routable: %s", result.Error)
	}
	if _, err := os.Lstat(filepath.Join(target, "agents", "bob")); !os.IsNotExist(err) {
		t.Fatalf("route explain repaired destination mailbox: %v", err)
	}

	runExplainedSend(t, result, "explicit unpinned cross-session")
	assertCompleteSendMailbox(t, target, "bob")
	if got := inboxCount(t, target, "bob"); got != 1 {
		t.Fatalf("delivered messages = %d, want 1", got)
	}
	for _, root := range []string{source, target} {
		if _, err := os.Lstat(filepath.Join(root, "meta", "config.json")); !os.IsNotExist(err) {
			t.Fatalf("send copied base config into session %s: %v", root, err)
		}
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || string(configAfter) != string(configBefore) {
		t.Fatalf("route/send changed base config: err=%v before=%q after=%q", err, configBefore, configAfter)
	}
}

func TestRouteExplainLocalNonRepairableMailboxRemainsUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := initializedSendMailboxRoot(t, "alice", "bob")
	outside := t.TempDir()
	unsafePath := fsq.AgentDLQCur(root, "bob")
	if err := os.Remove(unsafePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, unsafePath); err != nil {
		t.Fatal(err)
	}

	result := runRouteExplainJSONForTest(t,
		"--from-root", root,
		"--me", "alice",
		"--to", "bob",
	)
	if result.Routable {
		t.Fatalf("symlinked local mailbox was routable: %#v", result)
	}
	if !strings.Contains(result.Error, "symlink") {
		t.Fatalf("route error = %q, want symlink issue", result.Error)
	}
	info, err := os.Lstat(unsafePath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("route explain changed unsafe path: info=%v err=%v", info, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("route explain wrote through symlink: entries=%v err=%v", entries, err)
	}
}

func TestReplyRepairsConfiguredSameRootMissingMailbox(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	originalID := deliverOriginalForReply(t, root, "alice", format.Header{
		From:    "bob",
		To:      []string{"alice"},
		Thread:  "p2p/alice__bob",
		Subject: "local request",
	})
	if err := os.RemoveAll(filepath.Join(root, "agents", "bob")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", root,
			"--me", "alice",
			"--id", originalID,
			"--body", "local response",
			"--json",
		})
	}); err != nil {
		t.Fatalf("reply: %v", err)
	}

	assertCompleteSendMailbox(t, root, "bob")
	reply := soleDeliveredMessage(t, root, "bob")
	if reply.Header.Thread != "p2p/alice__bob" {
		t.Fatalf("reply thread = %q", reply.Header.Thread)
	}
	if len(reply.Header.Refs) != 1 || reply.Header.Refs[0] != originalID {
		t.Fatalf("reply refs = %v, want [%s]", reply.Header.Refs, originalID)
	}
}

func TestReplyRepairsCrossSessionMailboxUsingUnpinnedBaseOnlyCoopConfig(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	source := filepath.Join(base, "qa")
	target := filepath.Join(base, "dev")
	configureSendTestRoot(t, base, "alice", "bob")
	if err := fsq.EnsureAgentDirs(source, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(target, "bob"); err != nil {
		t.Fatal(err)
	}
	originalID := deliverOriginalForReply(t, source, "alice", format.Header{
		From:    "bob",
		To:      []string{"alice"},
		Thread:  "p2p/dev:bob__qa:alice",
		Subject: "cross-session request",
		ReplyTo: "bob@dev",
	})
	missing := fsq.AgentDLQCur(target, "bob")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", source,
			"--me", "alice",
			"--id", originalID,
			"--body", "cross-session response",
			"--json",
		})
	}); err != nil {
		t.Fatalf("reply: %v", err)
	}

	assertCompleteSendMailbox(t, target, "bob")
	reply := soleDeliveredMessage(t, target, "bob")
	if reply.Header.Thread != "p2p/dev:bob__qa:alice" {
		t.Fatalf("reply thread = %q", reply.Header.Thread)
	}
	if len(reply.Header.Refs) != 1 || reply.Header.Refs[0] != originalID {
		t.Fatalf("reply refs = %v, want [%s]", reply.Header.Refs, originalID)
	}
	if _, err := os.Lstat(filepath.Join(target, "meta", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("reply copied base config into target session: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(source, "meta", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("reply copied base config into source session: %v", err)
	}
}

func runExplainedSend(t *testing.T, result routeExplainResult, body string) {
	t.Helper()
	if len(result.Argv) < 2 || result.Argv[0] != "amq" || result.Argv[1] != "send" {
		t.Fatalf("route argv = %v, want amq send prefix", result.Argv)
	}
	args := append([]string(nil), result.Argv[2:]...)
	args = append(args, "--body", body, "--json")
	if _, _, err := captureEnvOutput(t, func() error {
		return runSend(args)
	}); err != nil {
		t.Fatalf("explained send: %v", err)
	}
}
