package launch

import (
	"fmt"
	"strings"
	"testing"
)

const testCodexCwd = "/tmp/codex-project"

func codexNotifyTestPayload(conversationID, cwd string) []byte {
	return []byte(fmt.Sprintf(`{"type":"agent-turn-complete","thread-id":%q,"turn-id":"turn-1","cwd":%q,"input-messages":["reply with ok"],"last-assistant-message":"ok"}`, conversationID, cwd))
}

func validCodexCapture() CaptureRequest {
	evidence, err := ParseCodexNotifyEvidence(codexNotifyTestPayload(testConversationID, testCodexCwd), testLaunchNonce, "codex", codexCaptureVersion, testCodexCwd)
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

func TestParseCodexNotifyEvidenceRejectsForgedEvents(t *testing.T) {
	valid := codexNotifyTestPayload(testConversationID, testCodexCwd)
	if _, err := ParseCodexNotifyEvidence(valid, testLaunchNonce, "codex", "0.148.0", testCodexCwd); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported expected version error = %v", err)
	}
	if _, err := ParseCodexNotifyEvidence(valid, testLaunchNonce, "codex", codexCaptureVersion, "/wrong"); err == nil || !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("wrong cwd error = %v", err)
	}

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"malformed", `{`, "decode"},
		{"wrong type", `{"type":"turn-started","thread-id":"` + testConversationID + `","turn-id":"turn-1","cwd":"` + testCodexCwd + `","input-messages":[]}`, "agent-turn-complete"},
		{"missing turn", `{"type":"agent-turn-complete","thread-id":"` + testConversationID + `","cwd":"` + testCodexCwd + `","input-messages":[]}`, "turn identity"},
		{"invalid id", `{"type":"agent-turn-complete","thread-id":"newest","turn-id":"turn-1","cwd":"` + testCodexCwd + `","input-messages":[]}`, "UUIDv7"},
		{"non-v7 id", `{"type":"agent-turn-complete","thread-id":"550e8400-e29b-41d4-a716-446655440000","turn-id":"turn-1","cwd":"` + testCodexCwd + `","input-messages":[]}`, "UUIDv7"},
		{"missing messages", `{"type":"agent-turn-complete","thread-id":"` + testConversationID + `","turn-id":"turn-1","cwd":"` + testCodexCwd + `"}`, "input-messages"},
		{"unknown field", `{"type":"agent-turn-complete","thread-id":"` + testConversationID + `","turn-id":"turn-1","cwd":"` + testCodexCwd + `","input-messages":[],"extra":true}`, "unknown field"},
		{"trailing event", string(valid) + ` {}`, "multiple JSON values"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseCodexNotifyEvidence([]byte(test.raw), testLaunchNonce, "codex", codexCaptureVersion, testCodexCwd); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseCodexNotifyEvidence error = %v, want %q", err, test.want)
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

func TestCursorCaptureRejectsForgedBinding(t *testing.T) {
	evidence, err := ParseCursorCreateChatEvidence([]byte(testConversationID), testLaunchNonce, "cursor", cursorCaptureVersion)
	if err != nil {
		t.Fatal(err)
	}
	request := CaptureRequest{LaunchNonce: testLaunchNonce, ExpectedProviderVersion: cursorCaptureVersion, Final: true, Evidence: []CaptureEvidence{evidence}}
	ready := captureCursorIdentity(request)
	if !ready.CanPersist() || ready.Identity != (ConversationIdentity{Provider: CursorProvider, ID: testConversationID}) {
		t.Fatalf("ready Cursor capture = %#v", ready)
	}
	request.Evidence[0].launchNonce = testConversationID
	forged := captureCursorIdentity(request)
	if forged.State != CaptureStale || forged.Reason != CaptureReasonLaunchNonceMismatch || forged.CanPersist() {
		t.Fatalf("forged Cursor capture = %#v", forged)
	}
}

func TestDecodeCursorCreateChatPayloadRejectsUnknownFields(t *testing.T) {
	raw := `{"source":"cursor_create_chat_v1","provider":"cursor-agent","provider_version":"2026.08.11-e8db854","launch_nonce":"` + testLaunchNonce + `","handle":"cursor","conversation_id":"` + testConversationID + `","stdout":"` + testConversationID + `","extra":true}`
	if _, err := decodeCursorCreateChatPayload([]byte(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}
