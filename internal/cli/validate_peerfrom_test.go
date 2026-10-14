package cli

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

func TestValidateHeaderAcceptsPeerSender(t *testing.T) {
	header := format.Header{
		Schema:   format.CurrentSchema,
		ID:       "hello-box",
		From:     "mac/codex",
		To:       []string{"grok"},
		Thread:   "p2p/codex__grok",
		Created:  "2026-10-14T19:00:00.000000000Z",
		Priority: "normal",
		Kind:     "status",
	}
	if err := validateHeaderFields(header); err != nil {
		t.Fatalf("peer From rejected: %v", err)
	}
	header.From = "mac/codex/extra"
	if err := validateHeaderFields(header); err == nil {
		t.Fatal("extra slash was accepted")
	}
}
