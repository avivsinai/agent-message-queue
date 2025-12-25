package format

import (
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:      1,
			ID:          "2025-12-24T15-02-33.123Z_pid1234_abcd",
			From:        "codex",
			To:          []string{"cloudcode"},
			Thread:      "p2p/codex__cloudcode",
			Subject:     "Hello",
			Created:     "2025-12-24T15:02:33.123Z",
			AckRequired: true,
			Refs:        []string{"ref1"},
		},
		Body: "Line one\nLine two\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Header.ID != msg.Header.ID {
		t.Fatalf("id mismatch: %s", parsed.Header.ID)
	}
	if parsed.Body != msg.Body {
		t.Fatalf("body mismatch: %q", parsed.Body)
	}
}
