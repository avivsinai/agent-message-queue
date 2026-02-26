package format

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Schema and version constants.
const (
	CurrentSchema  = 1
	CurrentVersion = 1
)

// MaxMessageSize is the maximum allowed message file size (10 MB).
const MaxMessageSize = 10 * 1024 * 1024

const (
	frontmatterStartLine = "---json"
	frontmatterStart     = frontmatterStartLine + "\n"
	frontmatterEnd       = "\n---\n"
)

// Sentinel errors for message parsing.
var (
	ErrMissingFrontmatterStart = errors.New("missing frontmatter start")
	ErrMissingFrontmatterEnd   = errors.New("missing frontmatter end")
	ErrMessageTooLarge         = errors.New("message exceeds maximum size")
)

// Priority constants for co-op mode message handling.
const (
	PriorityUrgent = "urgent"
	PriorityNormal = "normal"
	PriorityLow    = "low"
)

// Kind constants for co-op mode message classification.
const (
	KindBrainstorm     = "brainstorm"
	KindReviewRequest  = "review_request"
	KindReviewResponse = "review_response"
	KindQuestion       = "question"
	KindAnswer         = "answer"
	KindDecision       = "decision"
	KindStatus         = "status"
	KindTodo           = "todo"
	KindSpecResearch   = "spec_research"
	KindSpecDraft      = "spec_draft"
	KindSpecReview     = "spec_review"
	KindSpecDecision   = "spec_decision"
)

// Header is the JSON frontmatter stored at the top of each message file.
type Header struct {
	Schema      int      `json:"schema"`
	ID          string   `json:"id"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	Thread      string   `json:"thread"`
	Subject     string   `json:"subject,omitempty"`
	Created     string   `json:"created"`
	AckRequired bool     `json:"ack_required"`
	Refs        []string `json:"refs,omitempty"`

	// Co-op mode fields (optional, for inter-agent communication)
	Priority string         `json:"priority,omitempty"` // urgent, normal, low
	Kind     string         `json:"kind,omitempty"`     // brainstorm, review_request, review_response, question, answer, decision, status, todo
	Labels   []string       `json:"labels,omitempty"`   // free-form tags
	Context  map[string]any `json:"context,omitempty"`  // structured context (paths, symbols, etc.)
}

// Message is the in-memory representation of a message file.
type Message struct {
	Header Header
	Body   string
}

// ValidPriorities returns the list of valid priority values.
func ValidPriorities() []string {
	return []string{PriorityUrgent, PriorityNormal, PriorityLow}
}

// ValidKinds returns the list of valid kind values.
func ValidKinds() []string {
	return []string{KindBrainstorm, KindReviewRequest, KindReviewResponse, KindQuestion, KindAnswer, KindDecision, KindStatus, KindTodo, KindSpecResearch, KindSpecDraft, KindSpecReview, KindSpecDecision}
}

// IsValidPriority returns true if the priority is valid or empty.
func IsValidPriority(p string) bool {
	if p == "" {
		return true
	}
	for _, v := range ValidPriorities() {
		if p == v {
			return true
		}
	}
	return false
}

// IsValidKind returns true if the kind is valid or empty.
func IsValidKind(k string) bool {
	if k == "" {
		return true
	}
	for _, v := range ValidKinds() {
		if k == v {
			return true
		}
	}
	return false
}

func (m Message) Marshal() ([]byte, error) {
	if m.Header.Schema == 0 {
		m.Header.Schema = CurrentSchema
	}
	if m.Header.Created == "" {
		m.Header.Created = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.MarshalIndent(m.Header, "", "  ")
	if err != nil {
		return nil, err
	}
	body := m.Body
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	out := make([]byte, 0, len(frontmatterStart)+len(b)+len(frontmatterEnd)+len(body))
	out = append(out, frontmatterStart...)
	out = append(out, b...)
	out = append(out, frontmatterEnd...)
	out = append(out, body...)
	return out, nil
}

func ParseMessage(data []byte) (Message, error) {
	headerBytes, body, err := splitFrontmatter(data)
	if err != nil {
		return Message{}, err
	}
	var header Header
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Message{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	return Message{Header: header, Body: string(body)}, nil
}

func ParseHeader(data []byte) (Header, error) {
	headerBytes, _, err := splitFrontmatter(data)
	if err != nil {
		return Header{}, err
	}
	var header Header
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Header{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	return header, nil
}

func ReadMessageFile(path string) (Message, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Message{}, err
	}
	if info.Size() > MaxMessageSize {
		return Message{}, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Message{}, err
	}
	return ParseMessage(data)
}

func ReadHeaderFile(path string) (Header, error) {
	file, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer func() { _ = file.Close() }()
	return ReadHeader(file)
}

func ReadHeader(r io.Reader) (Header, error) {
	lr := io.LimitReader(r, MaxMessageSize)
	br := bufio.NewReader(lr)
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Header{}, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line != frontmatterStartLine {
		return Header{}, ErrMissingFrontmatterStart
	}
	if errors.Is(err, io.EOF) {
		return Header{}, ErrMissingFrontmatterEnd
	}

	dec := json.NewDecoder(br)
	var header Header
	if err := dec.Decode(&header); err != nil {
		return Header{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	rest := io.MultiReader(dec.Buffered(), br)
	if err := consumeFrontmatterEnd(bufio.NewReader(rest)); err != nil {
		return Header{}, err
	}
	return header, nil
}

func splitFrontmatter(data []byte) ([]byte, []byte, error) {
	if len(data) > MaxMessageSize {
		return nil, nil, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, len(data))
	}
	// Normalize CRLF to LF for cross-platform compatibility
	// (handles files edited on Windows or by editors that normalize line endings)
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))

	if !bytes.HasPrefix(data, []byte(frontmatterStart)) {
		return nil, nil, ErrMissingFrontmatterStart
	}
	payload := data[len(frontmatterStart):]
	dec := json.NewDecoder(bytes.NewReader(payload))
	var header json.RawMessage
	if err := dec.Decode(&header); err != nil {
		return nil, nil, fmt.Errorf("parse frontmatter json: %w", err)
	}
	rest := payload[dec.InputOffset():]
	rest = bytes.TrimLeft(rest, " \t\r\n")
	if !bytes.HasPrefix(rest, []byte("---\n")) {
		return nil, nil, ErrMissingFrontmatterEnd
	}
	body := rest[len("---\n"):]
	return header, body, nil
}

func consumeFrontmatterEnd(br *bufio.Reader) error {
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ErrMissingFrontmatterEnd
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "---" {
			return nil
		}
		return ErrMissingFrontmatterEnd
	}
}

// Timestamped is implemented by types that have a Created timestamp and ID for sorting.
type Timestamped interface {
	GetCreated() string
	GetID() string
	GetRawTime() time.Time
}

// SortByTimestamp sorts a slice of Timestamped items by time, then by ID for stability.
func SortByTimestamp[T Timestamped](items []T) {
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].GetRawTime(), items[j].GetRawTime()
		if !ti.IsZero() && !tj.IsZero() {
			if ti.Equal(tj) {
				return items[i].GetID() < items[j].GetID()
			}
			return ti.Before(tj)
		}
		ci, cj := items[i].GetCreated(), items[j].GetCreated()
		if ci == cj {
			return items[i].GetID() < items[j].GetID()
		}
		return ci < cj
	})
}
