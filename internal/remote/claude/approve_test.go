package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Bead 611.42.3: approve is offered only for a Bash command shown whole
// with nothing masked. A secret in the command is masked in the DM preview,
// which makes the call reject-only.
func TestApprovalPreviewMasksSecretsAndOffersApproveOnlyForPlainBash(t *testing.T) {
	if preview, ok := approvalPreview("Bash", json.RawMessage(`{"command":"go test ./...","description":"Run the tests"}`), ""); !ok || !strings.Contains(preview, "go test ./...") {
		t.Fatalf("plain Bash = %q, %v; want approvable with the command shown", preview, ok)
	}
	preview, ok := approvalPreview("Bash", json.RawMessage(`{"command":"curl -H 'Authorization: Bearer abcdef0123456789' https://example.test"}`), "")
	if ok || strings.Contains(preview, "abcdef0123456789") || !strings.Contains(preview, "[masked]") {
		t.Fatalf("Bash with a token = %q, %v; want the token masked and reject only", preview, ok)
	}
	// Pro review of #929, 2026-09-30, #4: quoted assignment and flag values,
	// and string secret fields of another tool's input.
	for _, tc := range []struct{ tool, input, secret string }{
		{"Bash", `{"command":"API_KEY=\"ordinary-demo-value\" ./check"}`, "ordinary-demo-value"},
		{"Bash", `{"command":"DATABASE_PASSWORD='ordinary demo value' ./check"}`, "demo value"},
		{"Bash", `{"command":"login --password \"ordinary demo value\""}`, "demo value"},
		{"Bash", `{"command":"export TOKEN='unterminated demo value"}`, "demo value"},
		{"WebFetch", `{"url":"https://example.test","api_key":"ordinary-demo-value"}`, "ordinary-demo-value"},
	} {
		preview, ok := approvalPreview(tc.tool, json.RawMessage(tc.input), "")
		if ok || strings.Contains(preview, tc.secret) {
			t.Fatalf("%s %s = %q, %v; want the value masked and reject only", tc.tool, tc.input, preview, ok)
		}
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
