package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestReportDeliveryErrorPreservesCommittedState(t *testing.T) {
	committed := &fsq.CommittedDurabilityError{
		FinalPath: "/delivery/agents/codex/inbox/new/msg.md",
		Recipient: "codex",
		Err:       errors.New("fsync failed"),
	}
	err := reportDeliveryError("msg", committed)
	if !errors.Is(err, committed) {
		t.Fatalf("reportDeliveryError did not preserve committed error: %v", err)
	}
	if !strings.Contains(err.Error(), "message msg has a committed delivery") || !strings.Contains(err.Error(), "retrying may duplicate") {
		t.Fatalf("reportDeliveryError = %q, want explicit committed retry warning", err)
	}
}
