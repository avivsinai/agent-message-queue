package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestApprovalPreviewDetectorTable is the one table for the DM preview's
// secret detector (bead 611.42.3). A hidden row returns only the hidden
// note; a visible row shows the command whole. Rows are only ever added,
// never removed: every case a review round hid stays hidden, and every
// benign case stays visible.
// demoBearer is built from parts so the source holds no credential-shaped
// literal for secret scanners; the detector still sees the full value.
var demoBearer = "abcdef" + "0123456789"

func TestApprovalPreviewDetectorTable(t *testing.T) {
	ghToken := "ghp_" + strings.Repeat("A", 36)
	bash := func(cmd string) string {
		raw, _ := json.Marshal(map[string]any{"command": cmd})
		return string(raw)
	}
	fallback := func(cmd string) string {
		raw, _ := json.Marshal(map[string]any{"command": cmd, "dangerouslyDisableSandbox": true})
		return string(raw)
	}
	for _, tc := range []struct {
		name, tool, input, agent string
		hidden                   bool
	}{
		// Benign calls stay visible.
		{"git status", "Bash", bash("git status --short"), "", false},
		{"ls -la", "Bash", bash("ls -la"), "", false},
		{"go test", "Bash", bash("go test ./... && rm -rf /tmp/work"), "", false},
		{"test named token", "Bash", bash("go test -run TestTokenExpiry ./..."), "", false},
		{"grep for token", "Bash", bash(`grep -R "token" .`), "", false},
		// Pro review of #929 r6, 2026-10-01: sk- inside a word is no token.
		{"task- file", "Bash", bash("ls docs/task-queue-controller.md"), "", false},
		{"task- package", "Bash", bash("go test ./internal/task-scheduler-engine"), "", false},
		{"disk- file", "Bash", bash("cat /var/log/disk-usage-report-weekly-summary-archive.txt"), "", false},
		{"risk- file", "Bash", bash("cat docs/risk-assessment-quarterly-review-summary.md"), "", false},

		// Pro review of #929, 2026-09-30, #4: quoted values and secret fields.
		{"quoted assignment", "Bash", bash(`API_KEY="ordinary-demo-value" ./check`), "", true},
		{"single-quoted assignment", "Bash", bash(`DATABASE_PASSWORD='ordinary demo value' ./check`), "", true},
		{"quoted flag value", "Bash", bash("login --pass" + `word "ordinary demo value"`), "", true},
		{"unterminated quote", "Bash", bash(`export TOKEN='unterminated demo value`), "", true},
		{"secret field", "WebFetch", `{"url":"https://example.test","api_key":"ordinary-demo-value"}`, "", true},
		{"bearer header", "Bash", bash("curl -H 'Authorization: Bearer " + demoBearer + "' https://example.test"), "", true},
		// Pro review of #929 r2, 2026-10-01, #3: the JSON fallback path.
		{"fallback quoted assignment", "Bash", fallback(`API_KEY="ordinary-demo-value" ./check`), "", true},
		{"fallback quoted flag", "Bash", fallback("login --pass" + `word "ordinary demo value"`), "", true},
		{"nested secret key", "WebFetch", `{"url":"https://example.test","config":{"headers":[{"apiToken":"ordinary-demo-value"}]}}`, "", true},
		// Pro review of #929 r3, 2026-10-01, #2.
		{"grep pattern quote", "Bash", bash(`cat /tmp/config | grep 'password: "' ; rm -rf /tmp/work`), "", true},
		{"fallback grep pattern quote", "Bash", fallback(`cat /tmp/config | grep 'password: "' ; rm -rf /tmp/work`), "", true},
		// Pro review of #929 r4, 2026-10-01, #3 and #5.
		{"interpreter program", "Bash", bash(`sh -c 'TOKEN=abc; rm -rf /tmp/work'`), "", true},
		{"flag before separator", "Bash", bash(`printf '%s\n' --token; rm -rf /tmp/work`), "", true},
		{"JSON payload", "Bash", bash(`curl --data '{"password":"demo-long-password"}' https://example.invalid`), "", true},
		{"fallback JSON payload", "Bash", fallback(`curl --data '{"password":"demo-long-password"}' https://example.invalid`), "", true},
		// Pro review of #929 r5, 2026-10-01, #2 and #3.
		{"identifier suffix", "Bash", bash("API_TOKEN_V2=ordinary-demo-value ./check"), "", true},
		{"fallback identifier suffix", "Bash", fallback("API_TOKEN_V2=ordinary-demo-value ./check"), "", true},
		{"secret word in key", "WebFetch", `{"url":"https://example.test","form":{"passwordConfirmation":"ordinary-demo-value"}}`, "", true},
		{"quoted flag spelling", "Bash", bash(`login --pass"word" "ordinary-demo-value"`), "", true},
		{"token in subagent", "Bash", bash("go test ./..."), "general " + ghToken, true},
		{"token after a long subagent label", "Bash", bash("go test ./..."), strings.Repeat("long-label ", 300) + ghToken, true},
		{"token in tool name", "mcp__" + ghToken, `{"query":"status"}`, "", true},
		{"nonbreaking space after bearer", "Bash", `{"command":"curl https://example.test","description":"Use Bearer` + "\u00a0" + demoBearer + `"}`, "", true},
		// Pro review of #929 r6, 2026-10-01: the restored name families.
		{"passphrase assignment", "Bash", bash("PASSPHRASE=ordinary-demo-value ./unlock"), "", true},
		{"passphrase flag", "Bash", bash("gpg --batch --pass" + "phrase ordinary-demo-value --decrypt document.gpg"), "", true},
		{"user flag", "Bash", bash("curl --us" + "er alice:ordinary-demo-value https://example.test"), "", true},
		{"fallback user flag", "Bash", fallback("curl --us" + "er alice:ordinary-demo-value https://example.test"), "", true},
		{"pwd key", "WebFetch", `{"url":"https://example.test","login":{"pwd":"ordinary-demo-value"}}`, "", true},
		{"session_id key", "WebFetch", `{"url":"https://example.test","state":{"session_id":"ordinary-demo-value"}}`, "", true},
		{"sessionid key", "WebFetch", `{"url":"https://example.test","state":{"sessionid":"ordinary-demo-value"}}`, "", true},
		{"session-id assignment", "Bash", bash("session-id=ordinary-demo-value ./check"), "", true},
		// Pro review of #929 r9, 2026-10-01: sk- inside any single-dash word.
		{"digit before user option", "Bash", bash("curl -4usk-AAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"fallback digit before user option", "Bash", fallback("curl -4usk-AAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"zero before user option", "Bash", bash("curl -0usk-AAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"fallback zero before user option", "Bash", fallback("curl -0usk-AAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"sign before user option", "Bash", bash("curl -#usk-AAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"fallback sign before user option", "Bash", fallback("curl -#usk-AAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		// Pro review of #929 r8, 2026-10-01: sk- is judged by its context.
		{"dated risk- file", "Bash", bash("cat docs/risk-assessment-quarterly-review-summary-2026.md"), "", false},
		{"capitalized Task- file", "Bash", bash("ls Task-Queue-Controller.md"), "", false},
		{"lowercase sk- token", "Bash", bash("curl -H x-api: sk-abcdefabcdefabcdefabcdefabcdefab https://example.test"), "", true},
		{"fallback lowercase sk- token", "Bash", fallback("curl -H x-api: sk-abcdefabcdefabcdefabcdefabcdefab https://example.test"), "", true},
		{"short sk- token", "Bash", bash("export OPENAI=sk-AbCdEf0123456789 && ./run"), "", true},
		{"fallback short sk- token", "Bash", fallback("export OPENAI=sk-AbCdEf0123456789 && ./run"), "", true},
		// Pro review of #929 r7, 2026-10-01: sk- tokens attached to short options.
		{"attached user option", "Bash", bash("curl -usk-AAAAAAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"fallback attached user option", "Bash", fallback("curl -usk-AAAAAAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"attached data option", "Bash", bash("curl -dsk-AAAAAAAAAAAAAAAAAAAAAAAA1 https://example.test"), "", true},
		{"fallback attached data option", "Bash", fallback("curl -dsk-AAAAAAAAAAAAAAAAAAAAAAAA1 https://example.test"), "", true},
		{"clustered user option", "Bash", bash("curl -vusk-AAAAAAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"fallback clustered user option", "Bash", fallback("curl -vusk-AAAAAAAAAAAAAAAAAAAAAAAA1: https://example.test"), "", true},
		{"clustered data option", "Bash", bash("curl -sdsk-AAAAAAAAAAAAAAAAAAAAAAAA1 https://example.test"), "", true},
		{"fallback clustered data option", "Bash", fallback("curl -sdsk-AAAAAAAAAAAAAAAAAAAAAAAA1 https://example.test"), "", true},
	} {
		preview := approvalPreview(tc.tool, json.RawMessage(tc.input), tc.agent)
		if tc.hidden && preview != previewHidden {
			t.Errorf("%s: preview = %.120q, want only the hidden note", tc.name, preview)
		}
		if !tc.hidden && (preview == previewHidden || preview == previewTooLong) {
			t.Errorf("%s: preview = %q, want the call shown", tc.name, preview)
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
