package board

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

type AnswerRequest struct{ Item, Option, Text string }

type AnswerResult struct {
	Recipient   string `json:"recipient,omitempty"`
	ID          string `json:"id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	CheckThread string `json:"check_thread,omitempty"`
}

// Select resolves only projected, owner-addressed questions. Card actions and
// commands in a client's request cannot authorize delivery.
func (q AnswerRequest) Select(inbox Inbox) (QuestionPlan, string, error) {
	var match *OwnerItem
	for i := range inbox.Items {
		item := &inbox.Items[i]
		if item.ID != q.Item {
			continue
		}
		if match != nil || item.question == nil {
			return QuestionPlan{}, "", errors.New("item is not a unique open owner question")
		}
		match = item
	}
	if match == nil {
		return QuestionPlan{}, "", errors.New("question is no longer open")
	}
	if q.Text != "" {
		return *match.question, q.Text, nil
	}
	for _, option := range match.Options {
		if option.Label == q.Option {
			return *match.question, option.Label, nil
		}
	}
	return QuestionPlan{}, "", errors.New("answer does not match the current options")
}

type SubmitAnswer func(context.Context, AnswerRequest) AnswerResult

func (l *Live) handleAnswer(w http.ResponseWriter, r *http.Request, host string) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "application/json")
	write := func(status int, result AnswerResult) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(result)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		write(http.StatusMethodNotAllowed, AnswerResult{Reason: "use POST"})
		return
	}
	if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != "http://"+host ||
		len(r.Header.Values("X-AMQ-Write-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-AMQ-Write-Token")), []byte(l.WriteToken)) != 1 {
		write(http.StatusForbidden, AnswerResult{Reason: "page authority expired or invalid"})
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || len(r.Header.Values("Content-Type")) != 1 {
		write(http.StatusUnsupportedMediaType, AnswerResult{Reason: "expected JSON"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, (16<<10)-1))
	if err != nil {
		write(http.StatusRequestEntityTooLarge, AnswerResult{Reason: "request exceeds the size limit or could not be read"})
		return
	}
	request, ok := decodeAnswer(raw)
	if !ok {
		write(http.StatusBadRequest, AnswerResult{Reason: "invalid answer request"})
		return
	}
	if len(request.Text) > 4<<10 {
		write(http.StatusBadRequest, AnswerResult{Reason: "the answer is over 4 KiB"})
		return
	}
	result := l.Submit(r.Context(), request)
	status := http.StatusOK
	if result.CheckThread != "" {
		status = http.StatusBadGateway
	} else if result.ID == "" {
		status = http.StatusConflict
	}
	write(status, result)
}

// Read exact, unique string fields. The default struct decoder accepts
// duplicate keys, case-insensitive keys and null string values.
func decodeAnswer(raw []byte) (AnswerRequest, bool) {
	if !utf8.Valid(raw) {
		return AnswerRequest{}, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return AnswerRequest{}, false
	}
	fields := map[string]string{}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return AnswerRequest{}, false
		}
		key, ok := k.(string)
		if !ok {
			return AnswerRequest{}, false
		}
		switch key {
		case "action", "item", "option", "text":
		default:
			return AnswerRequest{}, false
		}
		if _, exists := fields[key]; exists {
			return AnswerRequest{}, false
		}
		v, err := d.Token()
		value, ok := v.(string)
		if err != nil || !ok || strings.TrimSpace(value) == "" {
			return AnswerRequest{}, false
		}
		fields[key] = value
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return AnswerRequest{}, false
	}
	if _, err := d.Token(); err != io.EOF {
		return AnswerRequest{}, false
	}
	q := AnswerRequest{Item: fields["item"], Option: fields["option"], Text: fields["text"]}
	return q, fields["action"] == "answer" && q.Item != "" && (q.Option != "") != (q.Text != "")
}

// WithAnswerSnapshot serializes the published check and the caller's canonical
// read and reply across all serving processes for the project.
func (s Store) WithAnswerSnapshot(ctx context.Context, fn func(Snapshot)) error {
	if err := s.validateIdentity(ctx); err != nil {
		return err
	}
	if err := s.safeDirectory(); err != nil {
		return err
	}
	path := filepath.Join(s.Dir, "owner-answer.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("unsafe board answer lock")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return lock.WithExclusiveFileLockUntil(path, func() bool { return ctx.Err() != nil }, func() error {
		state, err := s.ReadContext(ctx)
		if err != nil {
			return err
		}
		fn(state)
		return nil
	})
}
