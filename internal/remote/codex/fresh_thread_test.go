package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// Bead agent-message-queue-611.54 (live, codex-cli 0.160.0): a loaded Codex
// thread with no turn yet failed attach with only "no rollout found"; the
// owner saw no target and no reason. The refusal now names the action. Review
// of #983 (Pro, P2): only that -32600 refusal is rewritten.
func TestAttachToAThreadWithNoTurnSaysWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		code   int
		action bool
	}{{-32600, true}, {-32603, false}} {
		sock, srv := startFakeAppServer(t)
		srv.resumeErrorMu.Lock()
		srv.resumeError = fmt.Sprintf(`{"code":%d,"message":"no rollout found for thread id t1"}`, tc.code)
		srv.resumeErrorMu.Unlock()
		cfg, _ := json.Marshal(map[string]string{"socket": sock, "thread": "t1"})
		_, err := Factory(context.Background(), registry.FactoryConfig{Config: cfg})
		if err == nil || strings.Contains(err.Error(), "send one prompt in that Codex session, then attach again") != tc.action {
			t.Fatalf("code %d: attach error = %v, want the send-one-prompt action: %v", tc.code, err, tc.action)
		}
	}
}
