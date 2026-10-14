package ring

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSlackWebhookPostsConstantText(t *testing.T) {
	var got slackBody
	var sawKey bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(RingKeyHeader) != "" {
			sawKey = true
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("decode body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	path := writeKeyFile(t, map[string]string{"url": server.URL})
	var stderr bytes.Buffer
	if err := RingSlack(path, "dot", "2", server.Client(), &stderr); err != nil {
		t.Fatalf("RingSlack: %v", err)
	}
	if got.Text != SlackText {
		t.Fatalf("text = %q, want %q", got.Text, SlackText)
	}
	if sawKey {
		t.Fatal("slack post carried a key header")
	}
	if strings.Contains(got.Text, "dot") || strings.Contains(got.Text, "2") {
		t.Fatalf("slack text leaked handle or pending: %q", got.Text)
	}
	if !strings.Contains(stderr.String(), ProgressAccepted) {
		t.Fatalf("stderr = %q, want accepted", stderr.String())
	}
}
