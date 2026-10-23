//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Bead agent-message-queue-vd4.4 (agy doorbell probe): the doorbell's Enter
// answered agy's "Run this command?" prompt. agy reports the prompt as
// OSC 7501 blocked:kind=permission; the relay holds the doorbell while it
// is open and shows the person agy's output without the status sequences.
func TestAgyStatusHoldsTheDoorbellWhilePermissionIsOpen(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "agy"); err != nil {
		t.Fatal(err)
	}
	sink, err := newAgyDialogSink(root, "agy", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	parser := &osc7501Parser{strip: true, onState: sink.onState}
	marker := filepath.Join(fsq.AgentBase(root, "agy"), wakeDialogFileName)

	shown := string(parser.feed([]byte("Run this command?\x1b]0;agy\x07\x1b]7501;state=blocked:kind=perm")))
	shown += string(parser.feed([]byte("ission:app=antigravity:msg=QQ==\x07 1. Yes")))
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("no dialog marker while agy asks for permission: %v", err)
	}
	shown += string(parser.feed([]byte("\x1b]7501;state=working:app=antigravity\x1b\\done")))
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("dialog marker after agy went back to work: %v", err)
	}
	if want := "Run this command?\x1b]0;agy\x07 1. Yesdone"; shown != want {
		t.Fatalf("shown %q, want %q", shown, want)
	}
}

// #1190 review: the relay deleted .wake.dialog unconditionally when it
// started, on idle and on exit, so it could remove a live owner's
// permission hold or a marker that replaced its own. It removes only the
// exact marker it wrote.
func TestAgyRelayKeepsMarkersItDidNotWrite(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "agy"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fsq.AgentBase(root, "agy"), wakeDialogFileName)
	foreign := []byte(`{"schema":1,"event":"PermissionRequest","opened_at":"x"}` + "\n")
	if err := os.WriteFile(marker, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	sink, err := newAgyDialogSink(root, "agy", nil)
	if err != nil {
		t.Fatal(err)
	}
	sink.onState("idle", "")
	if got, _ := os.ReadFile(marker); string(got) != string(foreign) {
		t.Fatalf("marker after relay start and idle = %q, want the foreign one", got)
	}
	sink.onState("blocked", "permission")
	if err := os.WriteFile(marker, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(marker); string(got) != string(foreign) {
		t.Fatalf("marker after relay exit = %q, want the one that replaced the relay's", got)
	}
}
