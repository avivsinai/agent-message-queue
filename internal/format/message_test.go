package format

import (
	"bytes"
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
			Schema:  1,
			ID:      "2025-12-24T15-02-33.123Z_pid1234_abcd",
			From:    "codex",
			To:      []string{"claude"},
			Thread:  "p2p/claude__codex",
			Subject: "Hello",
			Created: "2025-12-24T15:02:33.123Z",
			Refs:    []string{"ref1"},
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
			Schema:   1,
			ID:       "2025-12-27T10-00-00.000Z_pid5678_efgh",
			From:     "codex",
			To:       []string{"claude"},
			Thread:   "p2p/claude__codex",
			Subject:  "Review request",
			Created:  "2025-12-27T10:00:00.000Z",
			Priority: PriorityUrgent,
			Kind:     KindReviewRequest,
			Labels:   []string{"parser", "refactor"},
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

func TestValidKinds_ExactSet(t *testing.T) {
	expected := map[string]bool{
		"brainstorm":      true,
		"review_request":  true,
		"review_response": true,
		"question":        true,
		"answer":          true,
		"decision":        true,
		"status":          true,
		"todo":            true,
	}

	kinds := ValidKinds()
	if len(kinds) != len(expected) {
		t.Fatalf("expected %d kinds, got %d: %v", len(expected), len(kinds), kinds)
	}
	for _, k := range kinds {
		if !expected[k] {
			t.Errorf("unexpected kind in ValidKinds(): %q", k)
		}
	}

	// Spec kinds must NOT be valid (removed from core)
	for _, removed := range []string{"spec_research", "spec_draft", "spec_review", "spec_decision"} {
		if IsValidKind(removed) {
			t.Errorf("spec kind %q should not be valid after removal", removed)
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

func TestReadMessageFile(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:  1,
			ID:      "message-file-test",
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
	got, err := ReadMessageFile(path)
	if err != nil {
		t.Fatalf("ReadMessageFile: %v", err)
	}
	if got.Header.ID != "message-file-test" || got.Body != "Body content\n" {
		t.Fatalf("unexpected message: %+v", got)
	}
}

func TestReadMessageFileRejectsSymlink(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:  1,
			ID:      "symlink-message",
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
	target := filepath.Join(dir, "target.md")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err = ReadMessageFile(link)
	if err == nil {
		t.Fatal("expected symlink message file to be rejected")
	}
}

func TestReadHeaderFileRejectsSymlink(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:  1,
			ID:      "symlink-header",
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
	target := filepath.Join(dir, "target.md")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err = ReadHeaderFile(link)
	if err == nil {
		t.Fatal("expected symlink header file to be rejected")
	}
}

func TestMarshalFillsZeroSchemaAndEmptyCreated(t *testing.T) {
	msg := Message{
		Header: Header{
			ID:   "zero-schema",
			From: "codex",
			To:   []string{"claude"},
		},
		Body: "defaults\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Header.Schema != CurrentSchema {
		t.Fatalf("schema = %d, want %d", parsed.Header.Schema, CurrentSchema)
	}
	if parsed.Header.Created == "" {
		t.Fatal("created was left empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, parsed.Header.Created); err != nil {
		t.Fatalf("created %q is not RFC3339Nano: %v", parsed.Header.Created, err)
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
		_ = f.Close()
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()

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

func TestReadMessageFileAllowsExactMaxSize(t *testing.T) {
	_, data := mustExactMaxSizeMessage(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "exact.md")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write exact message: %v", err)
	}
	parsed, err := ReadMessageFile(path)
	if err != nil {
		t.Fatalf("ReadMessageFile exact MaxMessageSize: %v", err)
	}
	if parsed.Header.ID != "exact-max" || !strings.HasPrefix(parsed.Body, "pad\n") {
		t.Fatalf("parsed exact file = id=%q body[:8]=%q", parsed.Header.ID, parsed.Body)
	}
}

func TestParseMessageAllowsExactMaxSize(t *testing.T) {
	want, data := mustExactMaxSizeMessage(t)
	if len(data) != MaxMessageSize {
		t.Fatalf("padded size = %d, want %d", len(data), MaxMessageSize)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("ParseMessage exact MaxMessageSize: %v", err)
	}
	if parsed.Header.ID != want.Header.ID || parsed.Header.From != want.Header.From {
		t.Fatalf("header = %+v, want id=%q from=%q", parsed.Header, want.Header.ID, want.Header.From)
	}
	if !strings.HasPrefix(parsed.Body, "pad\n") {
		t.Fatalf("body prefix = %q, want pad\\n", parsed.Body[:min(8, len(parsed.Body))])
	}
}

func mustExactMaxSizeMessage(t *testing.T) (Message, []byte) {
	t.Helper()
	msg := Message{
		Header: Header{
			Schema:  CurrentSchema,
			ID:      "exact-max",
			From:    "codex",
			To:      []string{"claude"},
			Thread:  "p2p/claude__codex",
			Subject: "exact",
			Created: "2026-01-01T00:00:00.000000000Z",
		},
		Body: "pad\n",
	}
	seed, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if len(seed) > MaxMessageSize {
		t.Fatalf("seed size %d exceeds MaxMessageSize", len(seed))
	}
	data := make([]byte, MaxMessageSize)
	copy(data, seed)
	for i := len(seed); i < MaxMessageSize; i++ {
		data[i] = 'a'
	}
	return msg, data
}

func TestSortByTimestamp(t *testing.T) {
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

func TestSortByTimestampDoesNotCompareMixedZeroRawTimes(t *testing.T) {
	earlyCreated := "2025-01-01T00:00:00Z"
	lateCreated := "2025-01-02T00:00:00Z"
	headers := []testTimestamped{
		{id: "zero-raw-late-created", created: lateCreated},
		{id: "raw-early", created: earlyCreated, raw: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	SortByTimestamp(headers)

	if headers[0].id != "raw-early" || headers[1].id != "zero-raw-late-created" {
		t.Fatalf("order = [%s, %s], want raw-early then created-string fallback", headers[0].id, headers[1].id)
	}
}

func TestReplyProjectRoundTrip(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:       1,
			ID:           "xproj-test",
			From:         "claude",
			To:           []string{"codex"},
			Thread:       "p2p/proj-a:collab:claude__proj-b:collab:codex",
			Created:      "2026-03-19T00:00:00Z",
			ReplyTo:      "claude@collab",
			ReplyProject: "proj-a",
		},
		Body: "Cross-project hello\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Header.ReplyProject != "proj-a" {
		t.Errorf("reply_project mismatch: got %q, want %q", parsed.Header.ReplyProject, "proj-a")
	}
	if parsed.Header.ReplyTo != "claude@collab" {
		t.Errorf("reply_to mismatch: got %q, want %q", parsed.Header.ReplyTo, "claude@collab")
	}
}

func TestFromProjectRoundTrip(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:       1,
			ID:           "from-proj-test",
			From:         "claude",
			To:           []string{"claude"},
			Thread:       "p2p/homelab:s1:claude__yoetz:s1:claude",
			Created:      "2026-03-28T00:00:00Z",
			ReplyTo:      "claude@stream1",
			ReplyProject: "homelab-ai",
			FromProject:  "homelab-ai",
		},
		Body: "Cross-project same-handle message\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Header.FromProject != "homelab-ai" {
		t.Errorf("from_project mismatch: got %q, want %q", parsed.Header.FromProject, "homelab-ai")
	}
	if parsed.Header.ReplyProject != "homelab-ai" {
		t.Errorf("reply_project mismatch: got %q, want %q", parsed.Header.ReplyProject, "homelab-ai")
	}
}

func TestFromProjectOmittedWhenEmpty(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:  1,
			ID:      "no-from-proj",
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Created: "2026-03-28T00:00:00Z",
		},
		Body: "Local message\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(data, []byte("from_project")) {
		t.Error("from_project should be omitted when empty")
	}
}

func TestReplyProjectOmittedWhenEmpty(t *testing.T) {
	msg := Message{
		Header: Header{
			Schema:  1,
			ID:      "no-xproj",
			From:    "claude",
			To:      []string{"codex"},
			Created: "2026-03-19T00:00:00Z",
		},
		Body: "Local message\n",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "reply_project") {
		t.Error("expected reply_project to be omitted when empty")
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
