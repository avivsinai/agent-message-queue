package launch

import (
	"reflect"
	"testing"
)

// TestValidCodexConfigOverrideApprovalsReviewer covers the `-c
// approvals_reviewer` allowlist (issue #648 item 3). codex-cli 0.149.1 parses
// the value as TOML and accepts `user`, `auto_review`, and `guardian_subagent`
// in both quoted (`approvals_reviewer="auto_review"`) and bare
// (`approvals_reviewer=auto_review`) forms; an unknown value errors
// `unknown variant '<x>', expected one of 'user', 'auto_review',
// 'guardian_subagent'`. validCodexConfigOverride normalizes one surrounding
// quote pair and rejects unbalanced or inner quotes plus duplicate keys.
func TestValidCodexConfigOverrideApprovalsReviewer(t *testing.T) {
	accept := []string{
		`approvals_reviewer=user`,
		`approvals_reviewer=auto_review`,
		`approvals_reviewer=guardian_subagent`,
		`approvals_reviewer="user"`,
		`approvals_reviewer="auto_review"`,
		`approvals_reviewer="guardian_subagent"`,
	}
	for _, value := range accept {
		t.Run("accept/"+value, func(t *testing.T) {
			if !validCodexConfigOverride(value) {
				t.Fatalf("validCodexConfigOverride(%q) = false, want true", value)
			}
		})
	}

	reject := []string{
		`approvals_reviewer=`,
		`approvals_reviewer="auto_review`,
		`approvals_reviewer=auto_review"`,
		`approvals_reviewer="auto"review"`,
		`approvals_reviewer=rm -rf`,
		`approvals_reviewer=on_request`,
		`approvals_reviewer="on_request"`,
		`approvals_reviewer`,
		`=auto_review`,
		`approvals_reviewer=""`,
	}
	for _, value := range reject {
		t.Run("reject/"+value, func(t *testing.T) {
			if validCodexConfigOverride(value) {
				t.Fatalf("validCodexConfigOverride(%q) = true, want false", value)
			}
		})
	}
}

// TestValidateCodexConfigOverridesApprovalsReviewerSet asserts the full
// amq-squad "approve-for-me" override set accepts as a whole once
// approvals_reviewer is seeded. The issue records that the live set rejected as
// a whole and accepted only with this key removed; this reproduces the live
// emission, including the literal TOML quotes around the reviewer value.
func TestValidateCodexConfigOverridesApprovalsReviewerSet(t *testing.T) {
	set := []string{
		"-c", "model_reasoning_effort=high",
		"-c", `approvals_reviewer="auto_review"`,
	}
	if err := validateCodexConfigOverrides(set); err != nil {
		t.Fatalf("validateCodexConfigOverrides(%q) = %v, want nil", set, err)
	}

	bareSet := []string{
		"-c", "model_reasoning_effort=high",
		"-c", "approvals_reviewer=auto_review",
	}
	if err := validateCodexConfigOverrides(bareSet); err != nil {
		t.Fatalf("validateCodexConfigOverrides(%q) = %v, want nil", bareSet, err)
	}
}

// Reader compatibility: the daemon ticket grammar still accepts the two
// settings whose launch a daemon resumes refuses, so an old ticket round-trips
// for inspection and cleanup.
func TestCodexDaemonTicketGrammarReadsSandboxAndApproval(t *testing.T) {
	if got := codexDaemonArgumentsReason([]string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request"}); got != "" {
		t.Fatalf("older daemon settings must still parse for inspection, got refusal %q", got)
	}
}

// The terminal keeps its own options. Thread and daemon settings go in the
// daemon request instead of the argv whose resume would refuse them.
func TestCodexDaemonSettingsSplitIntoTerminalArgumentsAndPins(t *testing.T) {
	terminal, pins, reason := splitCodexDaemonArgs([]string{
		"--model", "m", "--sandbox", "read-only", "--ask-for-approval", "on-request",
		"-c", `model_reasoning_effort="low"`, "-c", "approvals_reviewer=user",
		"-c", "sandbox_workspace_write.network_access=false",
		"-c", "sandbox_workspace_write.writable_roots=[]", "--no-alt-screen",
	})
	want := CodexDaemonPins{Sandbox: "read-only", ApprovalPolicy: "on-request",
		ApprovalsReviewer: "user", ReasoningEffort: "low", NetworkAccess: "false", NoWritableRoots: true}
	if reason != "" || reflect.DeepEqual(terminal, []string{"--model", "m", "--no-alt-screen"}) == false || pins != want {
		t.Fatalf("split = %v, %+v, %q", terminal, pins, reason)
	}
	for _, tc := range []struct {
		options []string
		reason  string
	}{
		{[]string{"--search"}, "--search requires verified TUI resume equivalence"},
		{[]string{"-c", `sandbox_workspace_write.writable_roots=["__AMQ_MAILBOXES__"]`}, "the mailbox writable root needs an embedded app-server"},
	} {
		if _, _, got := splitCodexDaemonArgs(tc.options); got != tc.reason {
			t.Fatalf("split %v: reason %q; want %q", tc.options, got, tc.reason)
		}
	}
}

// Execution resolves the thread slot in the original ticket, then starts
// the terminal with the pinned settings left out of argv.
func TestCodexDaemonTerminalArgvLeavesThePinsOut(t *testing.T) {
	thread := "018f4e5a-1234-7abc-8def-0123456789ab"
	argv := []string{"codex", "--remote", "unix:///tmp/codex.sock", "-c", CodexNoUpdatePromptOverride,
		"resume", "-C", "/work", "--model", "m", "--sandbox", "read-only", "-c", "model_reasoning_effort=low", thread}
	ticket := ExecutionTicket{TargetArgv: argv, ConversationID: thread,
		DynamicArgv: []DynamicArg{{Index: len(argv) - 1, Kind: DynamicArgConversationID}}}
	got := CodexDaemonTerminalArgv(ticket, argv)
	want := []string{"codex", "--remote", "unix:///tmp/codex.sock", "-c", CodexNoUpdatePromptOverride, "resume", "-C", "/work", "--model", "m", thread}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal argv = %#v; want %#v", got, want)
	}
}
