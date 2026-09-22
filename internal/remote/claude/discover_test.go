package claude

import (
	"context"
	"os"
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
