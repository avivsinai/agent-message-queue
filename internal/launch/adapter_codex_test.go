package launch

import (
	"strings"
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

// TestValidateCodexConfigOverridesApprovalsReviewerDuplicateKey confirms a
// repeated approvals_reviewer key is rejected as duplicated, matching the
// existing model_reasoning_effort duplicate-key behavior.
func TestValidateCodexConfigOverridesApprovalsReviewerDuplicateKey(t *testing.T) {
	set := []string{
		"-c", "approvals_reviewer=user",
		"-c", `approvals_reviewer="auto_review"`,
	}
	err := validateCodexConfigOverrides(set)
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("validateCodexConfigOverrides(%q) = %v, want duplicated", set, err)
	}
}
