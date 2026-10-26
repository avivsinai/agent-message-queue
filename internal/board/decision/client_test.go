package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDecideUsesPinnedLocalTransportAndReturnsFiniteSelection(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	request := Request{
		Context:  "Review this evidence.",
		Evidence: []Unit{{ID: "u1", Text: "The request was answered.", Context: "mailbox", Allowed: []string{"task-1:update", "unresolved"}}},
		Interpretations: []Interpretation{
			{ID: "task-1:update", Description: "Attach this evidence to task 1 as an update."},
			{ID: "unresolved", Description: "The existing task cannot be identified.", Exclusive: true},
		},
	}
	var calls int
	var chatBody []byte
	wantCalls := []string{
		"GET /api/version", "GET /api/status", "GET /api/tags", "POST /api/chat",
		"GET /api/version", "GET /api/status", "GET /api/tags",
	}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls > len(wantCalls) || req.Method+" "+req.URL.Path != wantCalls[calls-1] {
			t.Fatalf("call %d = %s %s", calls, req.Method, req.URL.Path)
		}
		var body string
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/api/version":
			body = `{"version":"0.33.2"}`
		case req.Method == http.MethodGet && req.URL.Path == "/api/status":
			body = `{"cloud":{"disabled":true,"source":"env"}}`
		case req.Method == http.MethodGet && req.URL.Path == "/api/tags":
			body = fmt.Sprintf(`{"models":[{"name":"qwen3.5:4b","model":"qwen3.5:4b","modified_at":"2026-10-26T23:16:41.887551991+02:00","size":3324173934,"digest":"%s","details":{"parent_model":"model.gguf","format":"gguf","family":"qwen35","families":["qwen35"],"parameter_size":"4.2B","quantization_level":"Q4_K_M","context_length":262144,"embedding_length":2560},"capabilities":["completion","tools","thinking","vision"]}]}`, digest)
		case req.Method == http.MethodPost && req.URL.Path == "/api/chat":
			if req.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("chat content type = %q", req.Header.Get("Content-Type"))
			}
			payload, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			chatBody = payload
			var chat chatRequest
			if err := decodeStrict(payload, &chat); err != nil {
				t.Fatalf("decode chat request: %v", err)
			}
			if chat.Model != requestModel || chat.Stream || chat.Think || chat.Truncate || chat.Shift || chat.Options.NumCtx != 8192 || chat.Options.NumPredict != numPredict || chat.Options.Temperature != 0 {
				t.Fatalf("unexpected chat settings: %+v", chat)
			}
			if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || chat.Messages[1].Role != "user" || !strings.Contains(chat.Messages[1].Content, "The request was answered.") {
				t.Fatalf("unexpected chat messages: %+v", chat.Messages)
			}
			var schema struct {
				Properties map[string]struct {
					Items struct {
						Properties map[string]struct {
							Enum []string `json:"enum"`
						} `json:"properties"`
					} `json:"items"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(chat.Format, &schema); err != nil {
				t.Fatalf("decode output schema: %v", err)
			}
			if got := schema.Properties["selections"].Items.Properties["unit_id"].Enum; len(got) != 1 || got[0] != "u1" {
				t.Fatalf("output schema unit IDs = %v", got)
			}
			body = `{"model":"qwen3.5:4b:local","created_at":"2026-10-27T00:00:00Z","message":{"role":"assistant","content":"{\"selections\":[{\"unit_id\":\"u1\",\"choice_ids\":[\"task-1:update\"]}]}","thinking":"","tool_calls":[]},"done":true,"done_reason":"stop","prompt_eval_count":23,"eval_count":7,"total_duration":1,"load_duration":1,"prompt_eval_duration":1,"eval_duration":1}`
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})

	result, err := decideWithRoundTripper(context.Background(), Config{
		Endpoint: "http://127.0.0.1:11434", Model: profileModel, Digest: digest,
	}, request, rt)
	if err != nil {
		t.Fatal(err)
	}
	if calls != len(wantCalls) {
		t.Fatalf("request count = %d, want %d precheck/chat/postcheck calls", calls, len(wantCalls))
	}
	if len(result.Selections) != 1 || result.Selections[0].UnitID != "u1" || len(result.Selections[0].ChoiceIDs) != 1 || result.Selections[0].ChoiceIDs[0] != "task-1:update" {
		t.Fatalf("unexpected selections: %+v", result.Selections)
	}
	if result.RuntimeVersion != profileVersion || result.ModelDigest != digest || result.PromptHash != hashRequest(chatBody) || result.TokenMetrics.PromptEvalCount != 23 || result.TokenMetrics.EvalCount != 7 || result.Elapsed <= 0 {
		t.Fatalf("unexpected result metadata: %+v", result)
	}
}
