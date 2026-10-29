package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// Discovery lists a live interactive session that exposes a
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
		if err := os.WriteFile(filepath.Join(registryDir(defaultConfigDir(home)), "stale-"+strconv.Itoa(i)+".txt"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := discoverer{home: home}.Discover(context.Background(), registry.DiscoverRequest{})
	if !errors.Is(err, registry.ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete once the bound is hit", err)
	}
}

// A session run with CLAUDE_CONFIG_DIR attaches: discovery records the
// session's config dir, the endpoint reads the registry there, and the Stop
// hook still writes its marker under home. Regression for
// agent-message-queue-srt: attaching such a session was refused with "pid N
// has no session registry entry; refusing" because the endpoint read
// home/.claude/sessions.
func TestDiscoverAttachesSessionWithCustomConfigDir(t *testing.T) {
	if !noFollowSupported {
		t.Skip("stop hook receiver needs a no-follow open")
	}
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv(binding.EnvPath, "")
	pid := os.Getpid()
	const sid = "sess-custom"
	reg, _ := json.Marshal(sessionRegistry{Pid: pid, SessionID: sid, Kind: "interactive", MessagingSocketPath: "/tmp/cc-socks/x.sock"})
	regPath := filepath.Join(dir, "sessions", strconv.Itoa(pid)+".json")
	if err := os.MkdirAll(filepath.Dir(regPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regPath, reg, 0o600); err != nil {
		t.Fatal(err)
	}

	cands, err := discoverer{home: home}.Discover(context.Background(), registry.DiscoverRequest{})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Discover = %+v, %v; want one candidate", cands, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(cands[0].Config, &cfg); err != nil || cfg["config_dir"] != dir {
		t.Fatalf("config = %s, want config_dir %q", cands[0].Config, dir)
	}

	cfg["home"] = home
	raw, _ := json.Marshal(cfg)
	att, err := Factory(context.Background(), registry.FactoryConfig{Config: raw})
	if err != nil {
		t.Fatal(err)
	}
	if got := att.(*Attachment).boundSession; got != sid {
		t.Fatalf("attached session = %q, want %q", got, sid)
	}

	RunStopHookReceiver(home, strings.NewReader(`{"session_id":"`+sid+`","last_assistant_message":"done"}`), io.Discard)
	if line, err := os.ReadFile(stopMarkerPath(home, sid)); err != nil || !strings.Contains(string(line), `"text":"done"`) {
		t.Fatalf("stop marker = %q, %v; want the turn under home", line, err)
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && p != regPath {
			t.Errorf("AMQ wrote %s under the Claude config dir", p)
		}
		return nil
	})
}
