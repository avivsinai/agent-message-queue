package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	resumeT1 = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91b1"
	resumeT2 = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91b2"
)

func writeRollout(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Bead agent-message-queue-611.64: coop exec resolves `codex resume --last`
// once, with the TUI's own thread/list on the daemon, and binds that thread.
// Acceptance row 1: the selection is made once; a thread that becomes latest
// afterwards is never asked for.
func TestResolveResumeThreadSelectsTheLatestThreadAsCodexDoes(t *testing.T) {
	d := newFakeNamingDaemon(t)
	rollout := writeRollout(t, filepath.Join(t.TempDir(), "rollout.jsonl"))
	d.pages = []string{
		fmt.Sprintf(`{"data":[{"id":%q,"preview":"hi","path":%q}],"nextCursor":null}`, resumeT1, rollout),
		fmt.Sprintf(`{"data":[{"id":%q,"preview":"later","path":%q}],"nextCursor":null}`, resumeT2, rollout),
	}
	id, err := ResolveResumeThread(context.Background(), d.sock, ResumeQuery{Cwd: "/work", ConfigCwd: "/work"})
	if err != nil || id != resumeT1 {
		t.Fatalf("id=%q err=%v, want %s", id, err, resumeT1)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var methods []string
	for _, call := range d.calls {
		methods = append(methods, call.method)
	}
	if !reflect.DeepEqual(methods, []string{"initialize", "config/read", "thread/list"}) {
		t.Fatalf("requests %q, want initialize, config/read and one thread/list", methods)
	}
	// codex-cli 0.160.1 tui/src/lib.rs latest_session_lookup_params, first pass.
	var got, want map[string]any
	_ = json.Unmarshal(d.calls[2].params, &got)
	_ = json.Unmarshal([]byte(`{"limit":1,"sortKey":"updated_at","modelProviders":["openai"],"sourceKinds":["cli","vscode"],"archived":false,"cwd":"/work","useStateDbOnly":true}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("thread/list params %s", d.calls[2].params)
	}
}

// Bead agent-message-queue-611.64 acceptance row 3: two threads labelled
// s1/codex make the name ambiguous; Codex refuses it, and so does AMQ,
// naming both ids.
func TestResolveResumeThreadRefusesAnAmbiguousName(t *testing.T) {
	d := newFakeNamingDaemon(t)
	// Codex lists paths under the real home; macOS temp dirs are symlinks.
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	one := writeRollout(t, filepath.Join(home, "sessions", "a.jsonl"))
	two := writeRollout(t, filepath.Join(home, "sessions", "b.jsonl"))
	d.pages = []string{fmt.Sprintf(`{"data":[{"id":%q,"name":"s1/codex","preview":"","path":%q},{"id":%q,"name":"s1/codex","preview":"","path":%q}],"nextCursor":null}`, resumeT1, one, resumeT2, two)}
	id, err := ResolveResumeThread(context.Background(), d.sock, ResumeQuery{Name: "s1/codex", ConfigCwd: "/work", CodexHome: home})
	if err == nil || !strings.Contains(err.Error(), resumeT1) || !strings.Contains(err.Error(), resumeT2) {
		t.Fatalf("id=%q err=%v, want a refusal naming both threads", id, err)
	}
}

// Review of #1001 (P1): with CODEX_HOME set to a symlink, Codex lists thread
// paths under the real home, and AMQ refused every name.
func TestResolveResumeThreadNamesUnderASymlinkedCodexHome(t *testing.T) {
	d := newFakeNamingDaemon(t)
	home := t.TempDir()
	rollout := writeRollout(t, filepath.Join(home, "sessions", "a.jsonl"))
	link := filepath.Join(t.TempDir(), "codex-home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(rollout)
	if err != nil {
		t.Fatal(err)
	}
	d.pages = []string{fmt.Sprintf(`{"data":[{"id":%q,"name":"s1/codex","preview":"","path":%q}],"nextCursor":null}`, resumeT1, real)}
	id, err := ResolveResumeThread(context.Background(), d.sock, ResumeQuery{Name: "s1/codex", ConfigCwd: "/work", CodexHome: link})
	if err != nil || id != resumeT1 {
		t.Fatalf("id=%q err=%v, want %s", id, err, resumeT1)
	}
}
