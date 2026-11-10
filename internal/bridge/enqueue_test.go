package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func testBridgeMessage(t *testing.T, id, thread, from, body string) []byte {
	t.Helper()
	data, err := (format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      id,
			From:    from,
			To:      []string{"claude"},
			Thread:  thread,
			Created: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		},
		Body: body,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoadEnqueueConfigRejectsPermissiveAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	valid := `{
  "root": "` + dir + `",
  "source_host": "grok-host",
  "source_handle": "codex",
  "allowed_dest_aliases": ["mac/claude"]
}`

	permissive := filepath.Join(dir, "permissive.json")
	if err := os.WriteFile(permissive, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnqueueConfig(permissive); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("permissive config error = %v, want mode refusal", err)
	}

	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := LoadEnqueueConfig(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink config error = %v, want symlink refusal", err)
	}

	unknown := filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(unknown, []byte(strings.Replace(valid, "\n}", `,"root_override":"/tmp"}`+"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnqueueConfig(unknown); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestEnqueueWritesExactBytesToBridgeSpool(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	cfg := EnqueueConfig{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	}
	message := testBridgeMessage(t, "msg-enqueue", "thread-enqueue", "codex", "hello bridge")
	result, err := Enqueue(cfg, "mac/claude", message)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	want := filepath.Join(root, "bridge", "outbox", "codex", "new", "msg-enqueue.md")
	if result.Path != want {
		t.Fatalf("result path = %q, want %q", result.Path, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(message) {
		t.Fatalf("spool bytes changed: got %q, want %q", got, message)
	}
	info, err := os.Lstat(want)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("spool file mode = %v, want regular file", info.Mode())
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spool file perm = %o, want 0600", info.Mode().Perm())
	}
	destPath := DestSidecarPath(want)
	gotDest, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("dest sidecar: %v", err)
	}
	if strings.TrimSpace(string(gotDest)) != "mac/claude" {
		t.Fatalf("dest sidecar = %q, want mac/claude", gotDest)
	}
	destInfo, err := os.Lstat(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if destInfo.Mode()&os.ModeSymlink != 0 || destInfo.Mode().Perm() != 0o600 {
		t.Fatalf("dest sidecar mode = %v, want regular 0600", destInfo.Mode())
	}
}

// A retry after a crash left only the .dest sidecar publishes the message;
// a sidecar naming another destination still refuses (audit 2026-11-10).
func TestEnqueueRetryAfterCrashBetweenSidecarAndMessage(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	cfg := EnqueueConfig{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude", "mac/cursor"},
	}
	spool := filepath.Join(root, "bridge", "outbox", "codex", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "msg-retry.dest"), []byte("mac/claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	message := testBridgeMessage(t, "msg-retry", "thread-retry", "codex", "hello again")
	if _, err := Enqueue(cfg, "mac/cursor", message); err == nil {
		t.Fatal("Enqueue accepted a sidecar that names another destination")
	}
	if _, err := os.Lstat(filepath.Join(spool, "msg-retry.md")); !os.IsNotExist(err) {
		t.Fatalf("message published under another destination's sidecar: %v", err)
	}
	if _, err := Enqueue(cfg, "mac/claude", message); err != nil {
		t.Fatalf("retry Enqueue: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(spool, "msg-retry.md"))
	if err != nil || string(got) != string(message) {
		t.Fatalf("spool message = %q, %v; want the enqueued bytes", got, err)
	}
	if _, err := Enqueue(cfg, "mac/claude", message); err == nil {
		t.Fatal("Enqueue accepted a second copy of a published message")
	}
}

// A sidecar left after the courier archived its message to sent/ is not a
// retry: the enqueue refuses instead of sending the message twice
// (audit 2026-11-10).
func TestEnqueueRetryRefusesAMessageAlreadySent(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	cfg := EnqueueConfig{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	}
	message := testBridgeMessage(t, "msg-sent", "thread-sent", "codex", "sent once")
	outbox := filepath.Join(root, "bridge", "outbox", "codex")
	for _, dir := range []string{"new", "sent"} {
		if err := os.MkdirAll(filepath.Join(outbox, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outbox, "sent", "msg-sent.md"), message, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outbox, "new", "msg-sent.dest"), []byte("mac/claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Enqueue(cfg, "mac/claude", message); err == nil || !strings.Contains(err.Error(), "already sent") {
		t.Fatalf("Enqueue = %v, want an already-sent refusal", err)
	}
	if _, err := os.Lstat(filepath.Join(outbox, "new", "msg-sent.md")); !os.IsNotExist(err) {
		t.Fatalf("sent message was published again: %v", err)
	}
}

func TestEnqueueRejectsDestAllowlistAndSenderMismatch(t *testing.T) {
	root := t.TempDir()
	cfg := EnqueueConfig{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	}
	message := testBridgeMessage(t, "msg-deny", "thread-deny", "codex", "nope")
	if _, err := Enqueue(cfg, "mac/cursor", message); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("dest allowlist error = %v", err)
	}
	wrongSender := testBridgeMessage(t, "msg-deny", "thread-deny", "claude", "nope")
	if _, err := Enqueue(cfg, "mac/claude", wrongSender); err == nil || !strings.Contains(err.Error(), "source handle") {
		t.Fatalf("sender mismatch error = %v", err)
	}
}

func TestEnqueueRejectsInvalidFilename(t *testing.T) {
	root := t.TempDir()
	cfg := EnqueueConfig{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	}
	message := testBridgeMessage(t, "../escape", "thread-deny", "codex", "nope")
	if _, err := Enqueue(cfg, "mac/claude", message); err == nil || !strings.Contains(err.Error(), "filename") {
		t.Fatalf("invalid filename error = %v", err)
	}
}
