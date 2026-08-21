package launch

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestGrokLiveMintExitAndExactResume(t *testing.T) {
	if os.Getenv("AMQ_GROK_LIVE") != "1" {
		t.Skip("set AMQ_GROK_LIVE=1 to create, exit, and resume one real Grok session")
	}
	executable, err := exec.LookPath(GrokProvider)
	if err != nil {
		t.Skip("grok is not installed")
	}
	project := t.TempDir()
	conversationID := grokLiveUUID(t)
	first := runGrokLiveHeadless(t, executable, project, []string{
		"--no-auto-update", "--no-alt-screen", "--session-id", conversationID,
		"--output-format", "json", "-p", "Reply with exactly AMQ_GROK_MINT_OK.",
	})
	if !strings.Contains(first, "AMQ_GROK_MINT_OK") || !containsJSONString(first, conversationID) {
		t.Fatalf("Grok mint output did not prove the requested session ID: %s", first)
	}

	resumed := runGrokLiveHeadless(t, executable, project, []string{
		"--no-auto-update", "--no-alt-screen", "--resume", conversationID,
		"--output-format", "json", "-p", "Reply with exactly AMQ_GROK_RESUME_OK.",
	})
	if !strings.Contains(resumed, "AMQ_GROK_RESUME_OK") || !containsJSONString(resumed, conversationID) {
		t.Fatalf("Grok resume output did not prove exact session ID %s: %s", conversationID, resumed)
	}
}

func runGrokLiveHeadless(t *testing.T, executable, cwd string, args []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = cwd
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Grok live command %q: %v\n%s", args, err, output)
	}
	return string(output)
}

func grokLiveUUID(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func containsJSONString(raw, want string) bool {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return false
	}
	var visit func(any) bool
	visit = func(candidate any) bool {
		switch typed := candidate.(type) {
		case string:
			return typed == want
		case []any:
			for _, item := range typed {
				if visit(item) {
					return true
				}
			}
		case map[string]any:
			for _, item := range typed {
				if visit(item) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}
