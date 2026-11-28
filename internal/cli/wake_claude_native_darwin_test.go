//go:build darwin

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	const want = "<cross-session-message from=\"amq-doorbell\" from-session=\"amq\" from-name=\"amq-wake\" from-mode=\"code\">\n: AMQ doorbell run amq drain --for-turn then act on actionable mail\n</cross-session-message>"
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

// Bead upbr regression: the registry admits a newer Claude release that
// states peer protocol 1, and refuses protocol 2 with the named line.
func TestClaudeNativeRegistryAdmitsNewerReleaseOnPeerProtocol1(t *testing.T) {
	dir := t.TempDir()
	pid := os.Getpid()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRecord := func(version string, protocol int) {
		reg := claudeNativeRegistry{
			PID: pid, SessionID: "abc", Name: "s", Cwd: dir, Version: version,
			Protocol: protocol, Kind: "interactive", Socket: filepath.Join(dir, "sock"),
		}
		raw, _ := json.Marshal(reg)
		if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(pid)+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRecord("2.1.325", 1)
	binding := claudeNativeLaunch{SessionID: "abc", Cwd: dir, Home: dir, Launch: launch.HolderIdentity{PID: pid, ProcessStart: "x", BootID: "y"}}
	if _, err := readClaudeNativeRegistry(binding); err != nil {
		t.Fatalf("registry for release 2.1.325 protocol 1 must be admitted: %v", err)
	}
	writeRecord("2.1.325", 2)
	_, err := readClaudeNativeRegistry(binding)
	if err == nil {
		t.Fatal("registry for protocol 2 must be refused")
	}
	if !strings.Contains(err.Error(), "states peer protocol 2; claude-native speaks protocol 1") {
		t.Fatalf("wrong refusal text: %v", err)
	}
	if !errors.Is(err, errClaudeNativeRegistryMismatch) {
		t.Fatalf("a protocol mismatch must still classify as the registry mismatch that disables the injector: %v", err)
	}
}

// Live check, bead agent-message-queue-v9zg: a seat whose shell sets
// CLAUDE_CONFIG_DIR was refused. The launch's Claude home is that directory,
// and the session record is read from <dir>/sessions/<pid>.json.
func TestNativeConfigDirSelectsRegistryHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	pid := os.Getpid()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	reg := claudeNativeRegistry{
		PID: pid, SessionID: "abc", Name: "s", Cwd: "/tmp", Version: "2.1.325",
		Protocol: 1, Kind: "interactive", Socket: filepath.Join(dir, "sock"),
	}
	raw, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(pid)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prepare, err := prepareClaudeNativeLaunch(&launch.ExecutionTicket{
		Provider: launch.ClaudeProvider, Mode: launch.AdapterModeMint,
		ProviderVersion: launch.ClaudeNativeMinimumVersion,
		ConversationID:  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		LaunchNonce:     "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("prepare with CLAUDE_CONFIG_DIR set: %v", err)
	}
	if prepare.Home != dir {
		t.Fatalf("prepare.Home = %q; want the environment's directory %q", prepare.Home, dir)
	}
	// The session record is read from the launch's home.
	got, err := readClaudeSessionRecord(prepare.Home, pid)
	if err != nil || got.SessionID != "abc" {
		t.Fatalf("session record under the launch's home = %+v, %v; want session abc", got, err)
	}
}
