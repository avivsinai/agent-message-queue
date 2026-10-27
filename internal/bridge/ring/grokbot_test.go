package ring

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokbotWebhookPostsHandleAndPending(t *testing.T) {
	var gotHandle string
	var gotPending int
	var gotKey string
	var gotContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(RingKeyHeader)
		gotContent = r.Header.Get("Content-Type")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body grokbotBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotHandle = body.Handle
		gotPending = body.Pending
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	path := writeKeyFile(t, map[string]string{"url": server.URL, "key": "secret-ring"})
	var stderr bytes.Buffer
	if err := RingGrokbot(path, "grok", "3", server.Client(), &stderr); err != nil {
		t.Fatalf("RingGrokbot: %v", err)
	}
	if gotHandle != "grok" || gotPending != 3 {
		t.Fatalf("posted handle=%q pending=%d", gotHandle, gotPending)
	}
	if gotKey != "secret-ring" {
		t.Fatalf("key header = %q", gotKey)
	}
	if gotContent != "application/json" {
		t.Fatalf("content-type = %q", gotContent)
	}
	if !strings.Contains(stderr.String(), ProgressAccepted) {
		t.Fatalf("stderr = %q, want accepted", stderr.String())
	}
}

func TestGrokbotWebhookDefersOnServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	path := writeKeyFile(t, map[string]string{"url": server.URL, "key": "k"})
	var stderr bytes.Buffer
	err := RingGrokbot(path, "grok", "1", server.Client(), &stderr)
	if err == nil {
		t.Fatal("expected defer")
	}
	if !strings.Contains(stderr.String(), ProgressDeferred) {
		t.Fatalf("stderr = %q, want deferred", stderr.String())
	}
}

// The grokbot ring POST must send Authorization: Bearer <key> and keep
// X-AMQ-Ring-Key. Existing coverage only checked the custom header.
func TestGrokbotWebhookSendsBearerAndRingKey(t *testing.T) {
	const key = "ring-test-key-unique"
	var gotAuth, gotRingKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotRingKey = r.Header.Get(RingKeyHeader)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	path := writeKeyFile(t, map[string]string{"url": server.URL, "key": key})
	if err := RingGrokbot(path, "grok", "1", server.Client(), io.Discard); err != nil {
		t.Fatalf("RingGrokbot: %v", err)
	}
	if gotAuth != "Bearer "+key {
		t.Fatal("Authorization header was not Bearer <key>")
	}
	if gotRingKey != key {
		t.Fatal("X-AMQ-Ring-Key was missing or wrong")
	}
}

// A non-2xx ring response must not write the key to stderr or the error text.
func TestGrokbotWebhookNon2xxOmitsKeyFromLogsAndError(t *testing.T) {
	const key = "ring-key-must-not-appear"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	path := writeKeyFile(t, map[string]string{"url": server.URL, "key": key})
	var stderr bytes.Buffer
	err := RingGrokbot(path, "grok", "1", server.Client(), &stderr)
	if err == nil {
		t.Fatal("expected defer")
	}
	if strings.Contains(err.Error(), key) || strings.Contains(stderr.String(), key) {
		t.Fatal("ring key appeared in error text or logs")
	}
}

func writeKeyFile(t *testing.T, fields map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ring.json")
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
