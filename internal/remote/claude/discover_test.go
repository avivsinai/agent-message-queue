package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// 611.13: discovery lists a live interactive session that exposes a
// messaging socket, with the manifest config needed to attach it.
func TestDiscoverListsLiveSessionWithManifestConfig(t *testing.T) {
	pid := os.Getpid()
	home := tempHome(t, pid, &sessionRegistry{
		Pid: pid, SessionID: "s-live", Cwd: "/tmp/proj", Kind: "interactive",
		MessagingSocketPath: "/tmp/cc-socks/x.sock", Name: "main",
	})
	cands, err := discoverer{home: home}.Discover(context.Background(), registry.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Kind != "claude" || cands[0].Target != "claude:"+strconv.Itoa(pid) || cands[0].Display != "main" {
		t.Fatalf("candidates = %+v, want one claude candidate targeted claude:<pid> displayed main", cands)
	}
	if got, want := string(cands[0].Config), `{"pid":`+strconv.Itoa(pid)+`}`; got != want {
		t.Fatalf("config = %s, want %s", got, want)
	}
}

// codex #858 item 1: the scan stops at its bound and says so, keeping the
// candidates it found, instead of silently hiding sessions past the cap.
func TestDiscoverReportsIncompleteScanAtBound(t *testing.T) {
	pid := os.Getpid()
	home := tempHome(t, pid, &sessionRegistry{
		Pid: pid, SessionID: "s-live", Kind: "interactive", MessagingSocketPath: "/tmp/cc-socks/x.sock",
	})
	for i := 0; i < maxDiscoverEntries+5; i++ {
		if err := os.WriteFile(filepath.Join(claudeSessionsDir(home), "stale-"+strconv.Itoa(i)+".txt"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := discoverer{home: home}.Discover(context.Background(), registry.DiscoverRequest{})
	if !errors.Is(err, registry.ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete once the bound is hit", err)
	}
}
