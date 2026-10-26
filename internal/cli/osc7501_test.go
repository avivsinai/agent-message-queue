//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Bead agent-message-queue-vd4.4 (agy doorbell probe): the doorbell's Enter
// answered agy's "Run this command?" prompt. agy reports the prompt as
// OSC 7501 blocked:kind=permission; the relay holds the doorbell while it
// is open and shows the person agy's output without the status sequences.
// field: agy doorbell parked while busy (2026-10-26): working removed the
// marker, so the wake spent its attempts on a busy agy and never rang at
// the end of the turn. working holds the doorbell until done or idle.
func TestAgyStatusHoldsTheDoorbellWhileAPromptOrTurnIsOpen(t *testing.T) {
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

	// The steps run in order; kind is the marker kind after the step, ""
	// for no marker.
	steps := []struct {
		name   string
		chunks []string
		kind   string
	}{
		{"permission opens", []string{
			"Run this command?\x1b]0;agy\x07\x1b]7501;state=blocked:kind=perm",
			"ission:app=antigravity:msg=QQ==\x07 1. Yes",
		}, "permission"},
		{"working keeps", []string{"\x1b]7501;state=working:app=antigravity\x1b\\done"}, "working"},
		{"done clears", []string{"\x1b]7501;state=done:app=antigravity\x07"}, ""},
		{"working opens", []string{"\x1b]7501;state=working:app=antigravity\x07"}, "working"},
		{"idle clears", []string{"\x1b]7501;state=idle:app=antigravity\x07"}, ""},
	}
	var shown string
	for _, step := range steps {
		for _, chunk := range step.chunks {
			shown += string(parser.feed([]byte(chunk)))
		}
		raw, err := os.ReadFile(marker)
		switch {
		case step.kind == "" && !os.IsNotExist(err):
			t.Fatalf("%s: marker %q (%v), want none", step.name, raw, err)
		case step.kind != "" && err != nil:
			t.Fatalf("%s: no marker: %v", step.name, err)
		case step.kind != "":
			var got wakeDialogMarker
			if err := json.Unmarshal(raw, &got); err != nil || got.Kind != step.kind {
				t.Fatalf("%s: marker %q, want kind %q", step.name, raw, step.kind)
			}
		}
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
