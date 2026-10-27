//go:build darwin

package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// Native wake has a different admission path from authenticated Remote:
// its sole payload is the constant doorbell, never message content or keys.
func TestNativeWakeFrameContainsOnlyTheConstantDoorbell(t *testing.T) {
	raw, err := nativeDoorbellFrame("b759ed39-29b2-4260-95cf-15de40569466")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != 1 || raw[len(raw)-1] != '\n' {
		t.Fatalf("native frame must be a single JSON line: %q", raw)
	}
	var frame struct {
		Version  int                            `json:"msgV"`
		ID       string                         `json:"msg_id"`
		Type     string                         `json:"type"`
		Priority string                         `json:"priority"`
		Message  struct{ Role, Content string } `json:"message"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	const want = "<cross-session-message from=\"amq-doorbell\" from-session=\"amq\" from-name=\"amq-wake\" from-mode=\"code\">\n: AMQ doorbell run amq drain --include-body then act on it\n</cross-session-message>"
	if frame.Version != 1 || frame.ID != "b759ed39-29b2-4260-95cf-15de40569466" ||
		frame.Type != "user" || frame.Priority != "next" || frame.Message.Role != "user" || frame.Message.Content != want {
		t.Fatalf("unexpected native frame: %+v", frame)
	}
}

// The native uncertainty boundary must survive another wake scan and close
// only after the actual consumer emits its receipt. Socket reception and
// Claude startup are separate installed acceptance checks.
func TestNativeWakeWaitsForTheActualDrainReceipt(t *testing.T) {
	clearSendMailboxTestEnv(t)
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	h.now = time.Now().UTC().Add(-time.Second)
	h.send("native-held", format.PriorityNormal)
	owner, err := captureDarwinControlPeerOwner(os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := launch.HolderOf(owner.PID)
	if err != nil {
		t.Fatal(err)
	}
	wake, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	writeWakeLockForTest(t, h.root, "codex", wakeLock{PID: os.Getpid(), Generation: generation})
	dir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := withWakeLifecycleGuardInDir(dir, wakeGuardWait, func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	n := &claudeNativeNotifier{root: canonicalWakeRoot(h.root), agent: "codex", dir: dir,
		claim: inspectWakeLock(h.root, "codex"), owner: owner,
		binding: claudeNativeLaunch{SessionID: "b759ed39-29b2-4260-95cf-15de40569466", Launch: parent}, closed: make(chan struct{})}
	defer func() { _ = n.Close() }()
	n.state = claudeNativeAttempt{Schema: 1, Root: n.root, Agent: n.agent, Generation: generation,
		Wake: wake, Binding: n.binding, Phase: "unconfirmed", MessageID: "native-episode",
		CreatedAt: h.now, Cohort: []string{"native-held.md"}}
	if err := n.save(); err != nil {
		t.Fatal(err)
	}
	h.cfg.injectMode, h.cfg.native = wakeInjectModeClaudeNative, n
	h.scan(h.cfg)
	if n.state.Phase != "unconfirmed" || h.cfg.doorbell.phase != wakeDoorbellParked || len(h.injected) != 0 {
		t.Fatalf("unconfirmed attempt replayed or lost: phase=%s doorbell=%v input=%q", n.state.Phase, h.cfg.doorbell.phase, h.injected)
	}
	result := runDrainJSON(t, h.root, "codex", 20, true)
	if result.Count != 1 {
		t.Fatalf("drained %d messages, want 1", result.Count)
	}
	h.scan(h.cfg)
	var saved claudeNativeAttempt
	exists, err := n.readState(&saved)
	if err != nil || !exists || saved.Phase != "consumed" || h.cfg.doorbell.phase != wakeDoorbellIdle || len(h.injected) != 0 {
		t.Fatalf("drain not reconciled: saved=%+v exists=%v error=%v input=%q", saved, exists, err, h.injected)
	}
}
