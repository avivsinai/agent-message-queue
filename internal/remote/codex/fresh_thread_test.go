package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// Bead agent-message-queue-611.54 (live, codex-cli 0.160.0): a loaded Codex
// thread with no turn yet failed attach with only "no rollout found"; the
// owner saw no target and no reason. The refusal now names the action.
func TestAttachToAThreadWithNoTurnSaysWhatToDo(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	srv.resumeErrorMu.Lock()
	srv.resumeError = `{"code":-32600,"message":"no rollout found for thread id t1"}`
	srv.resumeErrorMu.Unlock()
	cfg, _ := json.Marshal(map[string]string{"socket": sock, "thread": "t1"})
	_, err := Factory(context.Background(), registry.FactoryConfig{Config: cfg})
	if err == nil || !strings.Contains(err.Error(), "send one prompt in that Codex session, then attach again") {
		t.Fatalf("attach error = %v, want the send-one-prompt action", err)
	}
}
