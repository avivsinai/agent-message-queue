package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Bead 611.42.3: the DM preview shows a call whole, or not at all. A call
// that may hold a secret anywhere shows only the hidden note; ❌ still
// blocks it.
func TestApprovalPreviewHidesACallThatMayHoldASecret(t *testing.T) {
	if preview := approvalPreview("Bash", json.RawMessage(`{"command":"go test ./... && rm -rf /tmp/work","description":"Run the tests"}`), ""); !strings.Contains(preview, "go test ./... && rm -rf /tmp/work") {
		t.Fatalf("plain Bash = %q, want the command shown whole", preview)
	}
	// Pro review of #929 r4, 2026-10-01, #3 and #5, and the earlier
	// masking cases: each hides the whole call.
	for _, tc := range []struct{ tool, input string }{
		{"Bash", `{"command":"sh -c 'TOKEN=abc; rm -rf /tmp/work'"}`},
		{"Bash", `{"command":"printf '%s\\n' --token; rm -rf /tmp/work"}`},
		{"Bash", `{"command":"curl --data '{\"password\":\"demo-long-password\"}' https://example.invalid"}`},
		{"Bash", `{"command":"curl --data '{\"password\":\"demo-long-password\"}' https://example.invalid","dangerouslyDisableSandbox":true}`},
		{"Bash", `{"command":"API_KEY=\"ordinary-demo-value\" ./check"}`},
		{"Bash", `{"command":"DATABASE_PASSWORD='ordinary demo value' ./check"}`},
		{"Bash", `{"command":"cat /tmp/config | grep 'password: \"' ; rm -rf /tmp/work"}`},
		{"Bash", `{"command":"curl -H 'Authorization: Bearer abcdef0123456789' https://example.test"}`},
		{"WebFetch", `{"url":"https://example.test","config":{"headers":[{"apiToken":"ordinary-demo-value"}]}}`},
	} {
		if preview := approvalPreview(tc.tool, json.RawMessage(tc.input), ""); preview != previewHidden {
			t.Fatalf("%s %s = %q, want the call hidden", tc.tool, tc.input, preview)
		}
	}
	// Pro review of #929 r5, 2026-10-01, #2 and #3: identifier suffixes, a
	// secret word inside a JSON key, a quoted flag spelling, a secret in the
	// subagent or tool name, and a nonbreaking space after Bearer. The note
	// stands alone, also after a label too long to show.
	ghToken := "ghp_" + strings.Repeat("A", 36)
	for _, tc := range []struct{ tool, input, agent string }{
		{"Bash", `{"command":"API_TOKEN_V2=ordinary-demo-value ./check"}`, ""},
		{"Bash", `{"command":"API_TOKEN_V2=ordinary-demo-value ./check","dangerouslyDisableSandbox":true}`, ""},
		{"WebFetch", `{"url":"https://example.test","form":{"passwordConfirmation":"ordinary-demo-value"}}`, ""},
		{"Bash", `{"command":"login --pass\"word\" \"ordinary-demo-value\""}`, ""},
		{"Bash", `{"command":"go test ./..."}`, "general " + ghToken},
		{"Bash", `{"command":"go test ./..."}`, strings.Repeat("long-label ", 300) + ghToken},
		{"mcp__" + ghToken, `{"query":"status"}`, ""},
		{"Bash", `{"command":"curl https://example.test","description":"Use Bearer abcdef0123456789"}`, ""},
	} {
		if preview := approvalPreview(tc.tool, json.RawMessage(tc.input), tc.agent); preview != previewHidden {
			t.Fatalf("%s %s (agent %.40q) = %.120q, want only the hidden note", tc.tool, tc.input, tc.agent, preview)
		}
	}
	long := `{"command":"echo ` + strings.Repeat("x", 3000) + `"}`
	if preview := approvalPreview("Bash", json.RawMessage(long), ""); preview != previewTooLong {
		t.Fatalf("long command = %q, want the too-long note", preview)
	}
}

// Bead 611.42.3, design section 5: install adds one PermissionRequest entry
// with no matcher, the AMQ_APPROVAL_HOOK marker, --wait 600 and timeout
// 630, beside the Stop hook; uninstall removes only that entry.
func TestInstallApprovalHookBesideStopHook(t *testing.T) {
	home := t.TempDir()
	if err := InstallStopHook(home, "/opt/amq-remote"); err != nil {
		t.Fatal(err)
	}
	if err := InstallPermissionHook(home, "/opt/amq-remote", DefaultPermissionWait); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(settingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := `{"hooks":[{"type":"command","command":"AMQ_APPROVAL_HOOK=1 '/opt/amq-remote' claude permission-hook --wait 600","timeout":630}]}`
	if len(got.Hooks["PermissionRequest"]) != 1 || string(got.Hooks["PermissionRequest"][0]) != want || len(got.Hooks["Stop"]) != 1 {
		t.Fatalf("settings = %s, want the Stop hook and %s", raw, want)
	}
	if state, err := PermissionHookState(home); err != nil || state != StopHookPresent {
		t.Fatalf("state = %q, %v; want installed", state, err)
	}
	if err := UninstallPermissionHook(home); err != nil {
		t.Fatal(err)
	}
	if state, _ := PermissionHookState(home); state != StopHookMissing {
		t.Fatalf("after uninstall the approval hook state = %q, want missing", state)
	}
	if state, _ := StopHookState(home); state != StopHookPresent {
		t.Fatalf("after uninstall the Stop hook state = %q, want it kept", state)
	}
}
