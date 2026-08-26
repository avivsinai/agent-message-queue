package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type recordingRunner struct {
	inner     CommandRunner
	lastQueue []byte
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.inner.Run(ctx, name, args...)
	if len(args) >= 1 && args[0] == "queue" && (len(args) < 2 || args[1] != "--help") {
		r.lastQueue = append([]byte(nil), out...)
	}
	return out, err
}

func TestCodexQueueLiveEnqueue(t *testing.T) {
	if os.Getenv("AMQ_CODEX_LIVE") != "1" {
		t.Skip("set AMQ_CODEX_LIVE=1 to queue into a GUI-held Codex thread")
	}
	codexPath, err := liveCodexPath()
	if err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	entries, err := os.ReadDir(filepath.Join(home, "thread-writer-locks"))
	if err != nil {
		t.Fatalf("read thread-writer-locks: %v", err)
	}
	insp := platformWriterLockInspector{}
	var uuid string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".lock") {
			continue
		}
		id := strings.TrimSuffix(name, ".lock")
		if !lowercaseThreadUUIDRe.MatchString(id) {
			continue
		}
		held, heldErr := insp.Held(context.Background(), filepath.Join(home, "thread-writer-locks", name))
		if heldErr != nil || !held {
			continue
		}
		uuid = id
		break
	}
	if uuid == "" {
		t.Fatal("no GUI-held thread-writer lock found; open a Codex app thread and retry")
	}
	t.Logf("live thread uuid=%s", uuid)
	rec := &recordingRunner{inner: ExecRunner{}}
	q := CodexQueue{
		Runner:   rec,
		LookPath: func(string) (string, error) { return codexPath, nil },
	}
	target := codexQueueTargetThreadPrefix + uuid
	if err := q.Inject(context.Background(), target, "AMQ_CODEX_LIVE probe: ignore"); err != nil {
		t.Fatalf("Inject() error = %v; queue output %q", err, rec.lastQueue)
	}
	got := string(bytes.TrimSpace(rec.lastQueue))
	t.Logf("queue success output: %s", got)
	if !strings.Contains(got, codexQueueSuccessMarker) {
		t.Fatalf("queue output %q does not contain success marker %q", got, codexQueueSuccessMarker)
	}
	if !strings.Contains(got, uuid) {
		t.Fatalf("queue output %q does not name thread %s", got, uuid)
	}
}

func liveCodexPath() (string, error) {
	var candidates []string
	if p, err := exec.LookPath("codex"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/opt/homebrew/bin/codex")
	for _, p := range candidates {
		out, err := exec.Command(p, "queue", "--help").CombinedOutput()
		if err == nil && strings.Contains(string(out), "--thread") && strings.Contains(string(out), "--message") {
			return p, nil
		}
	}
	return "", fmt.Errorf("no queue-capable codex; put a codex >= 0.149 first in PATH")
}
