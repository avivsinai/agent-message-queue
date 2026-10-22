package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testCodexCwd = "/tmp/codex-project"

func codexNotifyTestPayload(conversationID, cwd string) []byte {
	return []byte(fmt.Sprintf(`{"type":"agent-turn-complete","thread-id":%q,"turn-id":"turn-1","cwd":%q,"input-messages":["reply with ok"],"last-assistant-message":"ok"}`, conversationID, cwd))
}

// writeCodexRollout writes the first rollout line that codex-cli 0.163.0
// writes for a top-level TUI thread, in its local creation-date directory.
func writeCodexRollout(t *testing.T, codexHome, threadID, cwd string) {
	t.Helper()
	millis, err := strconv.ParseInt(strings.ReplaceAll(threadID[:13], "-", ""), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	created := time.UnixMilli(millis).Local()
	dir := filepath.Join(codexHome, "sessions", created.Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"timestamp":"2026-10-22T16:00:45.166Z","ordinal":0,"type":"session_meta","payload":{"session_id":%q,"id":%q,"cwd":%q,"originator":"codex-tui","cli_version":"0.163.0","source":"cli","thread_source":"user","history_mode":"paginated"}}`+"\n", threadID, threadID, cwd)
	name := "rollout-" + created.Format("2006-01-02T15-04-05") + "-" + threadID + ".jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validCodexCapture() CaptureRequest {
	evidence, err := ParseCodexNotifyEvidence(codexNotifyTestPayload(testConversationID, testCodexCwd), testLaunchNonce, "codex", codexCaptureMinimumVersion, testCodexCwd)
	if err != nil {
		panic(err)
	}
	return CaptureRequest{
		LaunchNonce: testLaunchNonce, ExpectedProviderVersion: "0.147.0", Final: true,
		Evidence: []CaptureEvidence{evidence},
	}
}

func TestCodexCaptureStateMachine(t *testing.T) {
	adapter := NewCodexAdapter("codex")
	pending := adapter.CaptureIdentity(CaptureRequest{
		LaunchNonce: testLaunchNonce, ExpectedProviderVersion: "0.147.0",
	})
	if pending.State != CapturePending || pending.Degraded || pending.CanPersist() {
		t.Fatalf("pending = %#v", pending)
	}
	ready := adapter.CaptureIdentity(validCodexCapture())
	if ready.State != CaptureReady || ready.Degraded || !ready.CanPersist() || ready.Identity.Provider != CodexProvider || ready.Identity.ID != testConversationID {
		t.Fatalf("ready = %#v", ready)
	}
	missing := validCodexCapture()
	missing.Evidence = nil
	stale := adapter.CaptureIdentity(missing)
	if stale.State != CaptureStale || !stale.Degraded || stale.CanPersist() || stale.Identity.ID != "" || stale.Reason != CaptureReasonEvidenceMissing {
		t.Fatalf("stale = %#v", stale)
	}
	unsupported := validCodexCapture()
	unsupported.Evidence[0].providerVersion = "0.146.0"
	result := adapter.CaptureIdentity(unsupported)
	if result.State != CaptureUnsupported || result.Degraded || result.CanPersist() || result.Reason != CaptureReasonProviderVersion {
		t.Fatalf("unsupported = %#v", result)
	}
}

func TestCodexCaptureRejectsAmbiguousOrForgedEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CaptureRequest)
		state  CaptureState
		reason CaptureReason
	}{
		{"ambiguous", func(request *CaptureRequest) {
			other := request.Evidence[0]
			other.conversationID = "018f1f2b-e465-75b8-87d7-21dddb678c13"
			request.Evidence = append(request.Evidence, other)
		}, CaptureStale, CaptureReasonEvidenceAmbiguous},
		{"forged nonce", func(request *CaptureRequest) { request.Evidence[0].launchNonce = testConversationID }, CaptureStale, CaptureReasonLaunchNonceMismatch},
		{"wrong provider", func(request *CaptureRequest) { request.Evidence[0].provider = ClaudeProvider }, CaptureStale, CaptureReasonProviderMismatch},
		{"newest file", func(request *CaptureRequest) { request.Evidence[0].source = "codex_newest_session_file" }, CaptureUnsupported, CaptureReasonEvidenceSource},
		{"invalid id", func(request *CaptureRequest) { request.Evidence[0].conversationID = "newest" }, CaptureStale, CaptureReasonInvalidIdentity},
		{"active elsewhere", func(request *CaptureRequest) { request.Evidence[0].activeElsewhere = true }, CaptureStale, CaptureReasonConversationActive},
		{"unverified struct", func(request *CaptureRequest) { request.Evidence[0].verified = false }, CaptureStale, CaptureReasonEvidenceUnverified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validCodexCapture()
			test.mutate(&request)
			result := NewCodexAdapter("codex").CaptureIdentity(request)
			if result.State != test.state || result.Reason != test.reason || result.CanPersist() || result.Identity.ID != "" {
				t.Fatalf("CaptureIdentity = %#v, want state %q reason %q and no identity", result, test.state, test.reason)
			}
		})
	}
}

func TestParseCursorCreateChatEvidenceRequiresOneCanonicalUUID(t *testing.T) {
	valid, err := ParseCursorCreateChatEvidence([]byte(testConversationID+"\n"), testLaunchNonce, "cursor", cursorCaptureVersion)
	if err != nil || valid.source != CursorCreateChatV1 || valid.handle != "cursor" || valid.conversationID != testConversationID {
		t.Fatalf("valid Cursor evidence = %#v, %v", valid, err)
	}
	tests := []struct {
		name    string
		raw     string
		version string
	}{
		{name: "empty", raw: "", version: cursorCaptureVersion},
		{name: "two lines", raw: testConversationID + "\n" + testLaunchNonce, version: cursorCaptureVersion},
		{name: "carriage return", raw: testConversationID + "\r\n", version: cursorCaptureVersion},
		{name: "surrounding space", raw: " " + testConversationID, version: cursorCaptureVersion},
		{name: "uppercase", raw: strings.ToUpper(testConversationID), version: cursorCaptureVersion},
		{name: "wrong version", raw: testConversationID, version: "2026.08.12-unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseCursorCreateChatEvidence([]byte(test.raw), testLaunchNonce, "cursor", test.version); err == nil {
				t.Fatal("ParseCursorCreateChatEvidence error = nil")
			}
		})
	}
}
