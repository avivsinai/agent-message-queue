package ring

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	ProgressAccepted = "AMQ_INJECT_PROGRESS=accepted"
	ProgressDeferred = "AMQ_INJECT_PROGRESS=deferred"
	maxResponseBytes = 4096
	RingKeyHeader    = "X-AMQ-Ring-Key"
)

type Poster interface {
	Do(*http.Request) (*http.Response, error)
}

func defaultPoster() Poster {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func postJSON(client Poster, rawURL, key string, body []byte) error {
	if client == nil {
		client = defaultPoster()
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: %w", ProgressDeferred, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(RingKeyHeader, key)
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", ProgressDeferred, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	return fmt.Errorf("%s: webhook status %d", ProgressDeferred, resp.StatusCode)
}

func writeProgress(stderr io.Writer, line string) {
	if stderr == nil {
		return
	}
	_, _ = io.WriteString(stderr, line)
	if !strings.HasSuffix(line, "\n") {
		_, _ = io.WriteString(stderr, "\n")
	}
}
