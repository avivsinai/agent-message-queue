package format

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMessageRoundTrip(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:      1,
			ID:          "2025-12-24T15-02-33.123Z_pid1234_abcd",
			From:        "codex",
			To:          []string{"claude"},
			Thread:      "p2p/claude__codex",
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
		{KindAnswer, true},
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

func TestParseMalformedFrontmatter_MissingStart(t *testing.T) {
	data := []byte(`{"id":"test"}` + "\n---\nHello\n")
	_, err := ParseMessage(data)
	if !errors.Is(err, ErrMissingFrontmatterStart) {
		t.Errorf("expected ErrMissingFrontmatterStart, got %v", err)
	}
}

func TestParseMalformedFrontmatter_MissingEnd(t *testing.T) {
	data := []byte("---json\n{\"id\":\"test\"}\nno closing delimiter\n")
	_, err := ParseMessage(data)
	if !errors.Is(err, ErrMissingFrontmatterEnd) {
		t.Errorf("expected ErrMissingFrontmatterEnd, got %v", err)
	}
}

func TestParseMalformedFrontmatter_CorruptJSON(t *testing.T) {
	data := []byte("---json\n{not valid json}\n---\nHello\n")
	_, err := ParseMessage(data)
	if err == nil {
		t.Fatal("expected error for corrupt JSON")
	}
	if !strings.Contains(err.Error(), "parse frontmatter") {
		t.Errorf("expected parse frontmatter error, got %v", err)
	}
}

func TestParseEmptyBody(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:  1,
			ID:      "test-empty-body",
			From:    "alice",
			To:      []string{"bob"},
			Created: "2025-01-01T00:00:00Z",
		},
		Body: "",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Body != "" {
		t.Errorf("expected empty body, got %q", parsed.Body)
	}
}

func TestParseCRLFNormalization(t *testing.T) {
	// Build a message with CRLF line endings
	raw := "---json\r\n{\"schema\":1,\"id\":\"crlf-test\",\"from\":\"alice\",\"to\":[\"bob\"],\"created\":\"2025-01-01T00:00:00Z\"}\r\n---\r\nHello CRLF\r\n"
	parsed, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse CRLF message: %v", err)
	}
	if parsed.Header.ID != "crlf-test" {
		t.Errorf("expected id crlf-test, got %s", parsed.Header.ID)
	}
	if !strings.Contains(parsed.Body, "Hello CRLF") {
		t.Errorf("expected body to contain 'Hello CRLF', got %q", parsed.Body)
	}
}

func TestReadHeader_Streaming(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:  1,
			ID:      "stream-test",
			From:    "alice",
			To:      []string{"bob"},
			Created: "2025-01-01T00:00:00Z",
		},
		Body: "Some body text\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := strings.NewReader(string(data))
	header, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if header.ID != "stream-test" {
		t.Errorf("expected id stream-test, got %s", header.ID)
	}
	if header.From != "alice" {
		t.Errorf("expected from alice, got %s", header.From)
	}
}

func TestReadHeaderFile(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:  1,
			ID:      "file-test",
			From:    "codex",
			To:      []string{"claude"},
			Created: "2025-01-01T00:00:00Z",
		},
		Body: "Body content\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	header, err := ReadHeaderFile(path)
	if err != nil {
		t.Fatalf("ReadHeaderFile: %v", err)
	}
	if header.ID != "file-test" {
		t.Errorf("expected id file-test, got %s", header.ID)
	}
}

func TestReadMessageFile_SizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.md")
	// Create a file larger than MaxMessageSize
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Write MaxMessageSize + 1 bytes
	if err := f.Truncate(MaxMessageSize + 1); err != nil {
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	_, err = ReadMessageFile(path)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("expected ErrMessageTooLarge, got %v", err)
	}
}

func TestSplitFrontmatter_SizeLimit(t *testing.T) {
	data := make([]byte, MaxMessageSize+1)
	_, err := ParseMessage(data)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("expected ErrMessageTooLarge, got %v", err)
	}
}

func TestSortByTimestamp(t *testing.T) {
	type item struct {
		id      string
		created string
		raw     time.Time
	}
	items := []item{
		{id: "c", created: "2025-01-03T00:00:00Z", raw: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)},
		{id: "a", created: "2025-01-01T00:00:00Z", raw: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{id: "b", created: "2025-01-02T00:00:00Z", raw: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)},
		{id: "d", created: "2025-01-02T00:00:00Z", raw: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)}, // same time as b, should sort by ID
	}

	type tsItem struct {
		item
	}
	wrappers := make([]tsItem, len(items))
	for i, it := range items {
		wrappers[i] = tsItem{it}
	}

	// Use a concrete type that implements Timestamped
	type sortable struct {
		id      string
		created string
		raw     time.Time
	}
	_ = sortable{} // just to verify type exists

	// Since SortByTimestamp requires Timestamped interface, test via Header adapter
	// We'll test the sort logic directly
	headers := []testTimestamped{
		{id: "c", created: "2025-01-03T00:00:00Z", raw: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)},
		{id: "a", created: "2025-01-01T00:00:00Z", raw: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{id: "b", created: "2025-01-02T00:00:00Z", raw: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)},
		{id: "d", created: "2025-01-02T00:00:00Z", raw: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)},
	}

	SortByTimestamp(headers)

	expected := []string{"a", "b", "d", "c"}
	for i, h := range headers {
		if h.id != expected[i] {
			t.Errorf("position %d: expected %s, got %s", i, expected[i], h.id)
		}
	}
}

// testTimestamped implements the Timestamped interface for testing.
type testTimestamped struct {
	id      string
	created string
	raw     time.Time
}

func (t testTimestamped) GetCreated() string    { return t.created }
func (t testTimestamped) GetID() string         { return t.id }
func (t testTimestamped) GetRawTime() time.Time { return t.raw }
