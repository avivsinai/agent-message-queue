// Package decision provides a bounded transport to one pinned local model.
// It returns candidate selections; callers retain all task and proposal authority.
package decision

import (
	"context"
	"time"
)

// Config identifies the supported local runtime and its caller-pinned model.
type Config struct {
	Endpoint string
	Model    string
	Digest   string
}

// Request contains untrusted material for a finite selection task.
type Request struct {
	Context         string           `json:"context"`
	Evidence        []Unit           `json:"evidence"`
	Interpretations []Interpretation `json:"interpretations"`
}

// Unit is one evidence item and the interpretation IDs allowed for it.
type Unit struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Context string   `json:"context"`
	Allowed []string `json:"allowed"`
}

// Interpretation is a possible selection.
type Interpretation struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Exclusive   bool   `json:"exclusive"`
}

// Selection records model output for one evidence unit.
type Selection struct {
	UnitID    string   `json:"unit_id"`
	ChoiceIDs []string `json:"choice_ids"`
}

// TokenMetrics are server-reported Ollama evaluation token counts.
type TokenMetrics struct {
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// Result contains validated model output and transport metadata.
type Result struct {
	Selections     []Selection   `json:"selections"`
	RuntimeVersion string        `json:"runtime_version"`
	ModelDigest    string        `json:"model_digest"`
	PromptHash     string        `json:"prompt_hash"`
	Elapsed        time.Duration `json:"elapsed"`
	TokenMetrics   TokenMetrics  `json:"token_metrics"`
}

// Decide asks the pinned local model for finite choices. Errors return an
// empty Result; no partial selection is usable.
func Decide(ctx context.Context, cfg Config, request Request) (Result, error) {
	return decideWithRoundTripper(ctx, cfg, request, nil)
}
