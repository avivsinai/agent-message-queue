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
			Thread:      "p2p/cloudcode__codex",
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

func TestMessageRoundTrip_CoopFields(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:      1,
			ID:          "2025-12-27T10-00-00.000Z_pid5678_efgh",
			From:        "codex",
			To:          []string{"claude"},
			Thread:      "p2p/claude__codex",
			Subject:     "Review request",
			Created:     "2025-12-27T10:00:00.000Z",
			AckRequired: true,
			Priority:    PriorityUrgent,
			Kind:        KindReviewRequest,
			Labels:      []string{"parser", "refactor"},
			Context: map[string]any{
				"paths": []any{"internal/format/message.go"},
				"focus": "error handling",
			},
		},
		Body: "Please review this code.\n",
	}

	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Verify co-op fields
	if parsed.Header.Priority != PriorityUrgent {
		t.Errorf("priority mismatch: expected %s, got %s", PriorityUrgent, parsed.Header.Priority)
	}
	if parsed.Header.Kind != KindReviewRequest {
		t.Errorf("kind mismatch: expected %s, got %s", KindReviewRequest, parsed.Header.Kind)
	}
	if len(parsed.Header.Labels) != 2 {
		t.Errorf("labels count mismatch: expected 2, got %d", len(parsed.Header.Labels))
	}
	if parsed.Header.Labels[0] != "parser" || parsed.Header.Labels[1] != "refactor" {
		t.Errorf("labels mismatch: %v", parsed.Header.Labels)
	}
	if parsed.Header.Context == nil {
		t.Fatal("context is nil")
	}
	if parsed.Header.Context["focus"] != "error handling" {
		t.Errorf("context.focus mismatch: %v", parsed.Header.Context["focus"])
	}
}

func TestValidPriority(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"", true},
		{PriorityUrgent, true},
		{PriorityNormal, true},
		{PriorityLow, true},
		{"invalid", false},
		{"URGENT", false}, // case-sensitive
	}

	for _, tc := range tests {
		got := IsValidPriority(tc.input)
		if got != tc.valid {
			t.Errorf("IsValidPriority(%q) = %v, want %v", tc.input, got, tc.valid)
		}
	}
}

func TestValidKind(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"", true},
		{KindReviewRequest, true},
		{KindReviewResponse, true},
		{KindQuestion, true},
		{KindBrainstorm, true},
		{KindDecision, true},
		{KindStatus, true},
		{KindTodo, true},
		{"invalid", false},
		{"REVIEW_REQUEST", false}, // case-sensitive
	}

	for _, tc := range tests {
		got := IsValidKind(tc.input)
		if got != tc.valid {
			t.Errorf("IsValidKind(%q) = %v, want %v", tc.input, got, tc.valid)
		}
	}
}
