package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const unreadBacklogNoNotifierRemedy = "drain from the owning session"

func writeUnreadBacklogForTest(t *testing.T, root string, age time.Duration) {
	t.Helper()
	messagePath := filepath.Join(fsq.AgentInboxNew(root, "alice"), "message.md")
	if err := os.WriteFile(messagePath, []byte("message"), 0o600); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(messagePath, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorOpsUnreadBacklogNoNotifierHintMatrix(t *testing.T) {
	tests := []struct {
		name           string
		unread         bool
		lockState      string
		wantHint       bool
		wantLockStatus string
	}{
		{name: "backlog without lock", unread: true, wantHint: true},
		{name: "backlog with stale lock", unread: true, lockState: "stale", wantHint: true, wantLockStatus: string(wakeLockStale)},
		{name: "backlog with unverified lock", unread: true, lockState: "unverified", wantLockStatus: string(wakeLockUnverified)},
		{name: "backlog with live lock", unread: true, lockState: "live"},
		{name: "no backlog without lock"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := healthyDoctorMailboxRoot(t, "alice")
			if test.unread {
				writeUnreadBacklogForTest(t, root, 90*time.Second)
			}
			if test.lockState != "" {
				const pid = 4242
				const processStart = "verified-start"
				const bootID = "verified-boot"
				args := []string{"amq", "wake", "--root", root, "--me", "alice"}
				lock := wakeLock{
					PID:          pid,
					ProcessStart: processStart,
					BootID:       bootID,
					Executable:   "amq",
					Args:         args,
				}
				if test.lockState == "unverified" {
					lock.ProcessStart = ""
					lock.BootID = ""
				}
				writeWakeLockForTest(t, root, "alice", lock)
				stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
					if test.lockState == "stale" {
						return wakeProcessInfo{PID: gotPID, Running: false}
					}
					if test.lockState == "unverified" {
						return wakeProcessInfo{PID: gotPID, Running: true, Executable: "/usr/local/bin/amq"}
					}
					return wakeProcessInfo{
						PID:        gotPID,
						Running:    true,
						StartToken: processStart,
						BootID:     bootID,
						Executable: "/usr/local/bin/amq",
						Args:       args,
					}
				})
			}

			result := runOpsChecks(root, "test", false)
			hint, found := findOpsHint(result.Hints, "unread_backlog_no_notifier")
			if found != test.wantHint {
				t.Fatalf("hint present = %v, want %v: %#v", found, test.wantHint, result.Hints)
			}
			if test.lockState != "" && (len(result.WakeLocks) != 1 || result.WakeLocks[0].NotifierAbsent != test.wantHint) {
				t.Fatalf("semantic notifier absence = %#v, want %v", result.WakeLocks, test.wantHint)
			}
			if !test.wantHint {
				if test.wantLockStatus != "" && (len(result.WakeLocks) != 1 || result.WakeLocks[0].Status != test.wantLockStatus) {
					t.Fatalf("wake lock status = %#v, want %q", result.WakeLocks, test.wantLockStatus)
				}
				if test.lockState == "live" && (len(result.Agents) != 1 || result.Agents[0].PresenceSource != presenceSourceNotifierLive) {
					t.Fatalf("live fixture presence source = %#v", result.Agents)
				}
				return
			}

			if hint.Status != "warn" || hint.UnreadBacklogNoNotifier == nil {
				t.Fatalf("hint = %#v", hint)
			}
			payload := hint.UnreadBacklogNoNotifier
			if payload.Agent != "alice" || payload.UnreadCount != 1 {
				t.Fatalf("identity/count = %#v", payload)
			}
			if payload.OldestUnreadAgeSeconds < 89 || payload.OldestUnreadAgeSeconds > 92 {
				t.Fatalf("oldest unread age = %v, want about 90", payload.OldestUnreadAgeSeconds)
			}
			wantCommand := "amq wake check --root " + shellQuoteArg(root) + " --me alice"
			if payload.Command != wantCommand {
				t.Fatalf("command = %q, want %q", payload.Command, wantCommand)
			}
			if payload.Remedy != unreadBacklogNoNotifierRemedy {
				t.Fatalf("remedy = %q", payload.Remedy)
			}
			encoded, err := json.Marshal(hint)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{`"code":"unread_backlog_no_notifier"`, `"agent":"alice"`, `"unread_count":1`, `"oldest_unread_age_seconds":`, `"command":"amq wake check --root `, `"remedy":"drain from the owning session"`} {
				if !strings.Contains(string(encoded), want) {
					t.Fatalf("JSON hint missing %q: %s", want, encoded)
				}
			}
		})
	}
}

func TestDoctorOpsOutputsIncludeUnreadBacklogNoNotifierHint(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "alice")
	writeUnreadBacklogForTest(t, root, 90*time.Second)
	clearDoctorSessionPin(t)

	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--root", root, "--ops"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"alice has 1 unread message",
		"oldest 90s",
		"amq wake check --root " + shellQuoteArg(root) + " --me alice",
		unreadBacklogNoNotifierRemedy,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("human output missing %q:\n%s", want, output)
		}
	}

	output, err = captureEnvStdout(t, func() error {
		return runDoctor([]string{"--root", root, "--ops", "--json", "--json-schema=2"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result doctorResultV2
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Ops == nil {
		t.Fatal("v2 ops result missing")
	}
	hint, found := findOpsHint(result.Ops.Hints, "unread_backlog_no_notifier")
	if !found || hint.UnreadBacklogNoNotifier == nil ||
		!strings.Contains(hint.UnreadBacklogNoNotifier.Command, "--root ") {
		t.Fatalf("v2 JSON hint missing or not root-qualified: %#v", result.Ops.Hints)
	}
}

func TestDoctorOpsV2UnreadBacklogNoNotifierAfterStaleLockRemoval(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "alice")
	writeUnreadBacklogForTest(t, root, 0)
	lockPath := writeWakeLockForTest(t, root, "alice", wakeLock{
		PID:        4242,
		Executable: "/usr/local/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	result := runOpsChecksWithSchema(root, "test", true, wakeCheckSchemaV2)
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock was not removed: %v", err)
	}
	if len(result.WakeLocks) != 1 || result.WakeLocks[0].Mutation == nil || !result.WakeLocks[0].Mutation.Removed {
		t.Fatalf("v2 removal result = %#v", result.WakeLocks)
	}
	if !result.WakeLocks[0].NotifierAbsent {
		t.Fatalf("v2 removal lost semantic notifier absence: %#v", result.WakeLocks[0])
	}
	if result.WakeLocks[0].WakeCheckDecision == nil || result.WakeLocks[0].WakeCheckDecision.Wake.Status != string(wakeLockMissing) {
		t.Fatalf("v2 wake decision = %#v", result.WakeLocks[0].WakeCheckDecision)
	}
	if _, found := findOpsHint(result.Hints, "unread_backlog_no_notifier"); !found {
		t.Fatalf("v2 removed stale lock did not emit hint: %#v", result.Hints)
	}
}

func TestUnreadBacklogNoNotifierUsesCurrentSemanticWakeState(t *testing.T) {
	agent := opsAgent{Handle: "alice", UnreadCount: 1, OldestUnreadAgeSeconds: 90}
	tests := []struct {
		name     string
		lock     opsWakeLock
		wantHint bool
	}{
		{
			name: "v1 removed mutation followed by live replacement",
			lock: opsWakeLock{
				Agent:          "alice",
				Status:         "fixed",
				Removed:        true,
				NotifierAbsent: false,
			},
		},
		{
			name: "v2 removed mutation followed by live replacement",
			lock: opsWakeLock{
				Agent:          "alice",
				Status:         string(wakeLockValid),
				Mutation:       &opsWakeMutation{Status: "fixed", Removed: true},
				NotifierAbsent: false,
				WakeCheckDecision: &wakeCheckDecision{
					Wake: wakeCheckWakeDecision{Status: string(wakeLockValid), Live: true},
				},
			},
		},
		{
			name: "v1 failed removal retains proven stale evidence",
			lock: opsWakeLock{
				Agent:          "alice",
				Status:         "error",
				Mutation:       &opsWakeMutation{Status: "error"},
				NotifierAbsent: true,
			},
			wantHint: true,
		},
		{
			name: "v2 failed removal retains proven stale evidence",
			lock: opsWakeLock{
				Agent:          "alice",
				Status:         string(wakeLockStale),
				Mutation:       &opsWakeMutation{Status: "error"},
				NotifierAbsent: true,
				WakeCheckDecision: &wakeCheckDecision{
					Wake: wakeCheckWakeDecision{Status: string(wakeLockStale)},
				},
			},
			wantHint: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hints := checkUnreadBacklogNoNotifierHints(t.TempDir(), []opsAgent{agent}, []opsWakeLock{test.lock})
			_, found := findOpsHint(hints, "unread_backlog_no_notifier")
			if found != test.wantHint {
				t.Fatalf("hint present = %v, want %v: %#v", found, test.wantHint, hints)
			}
		})
	}
}

func TestDoctorOpsV1ChangedBeforeFixDoesNotClaimNotifierAbsent(t *testing.T) {
	tests := []struct {
		name        string
		replacement wakeProcessInfo
		wantStatus  string
	}{
		{
			name: "live replacement",
			replacement: wakeProcessInfo{
				PID:        4242,
				Running:    true,
				StartToken: "verified-start",
				BootID:     "verified-boot",
				Executable: "/usr/local/bin/amq",
				Args:       []string{"amq", "wake"},
			},
			wantStatus: string(wakeLockValid),
		},
		{
			name: "unverified replacement",
			replacement: wakeProcessInfo{
				PID:          4242,
				Running:      true,
				InspectError: errors.New("permission denied"),
			},
			wantStatus: string(wakeLockUnverified),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := healthyDoctorMailboxRoot(t, "alice")
			writeUnreadBacklogForTest(t, root, 0)
			writeWakeLockForTest(t, root, "alice", wakeLock{
				PID:          4242,
				ProcessStart: "verified-start",
				BootID:       "verified-boot",
				Executable:   "amq",
				Args:         []string{"amq", "wake"},
			})
			calls := 0
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				calls++
				if calls < 3 {
					return wakeProcessInfo{PID: pid, Running: false}
				}
				return test.replacement
			})

			result := runOpsChecks(root, "test", true)
			if calls < 3 {
				t.Fatalf("fixture did not reach guarded recheck: %d calls", calls)
			}
			if len(result.WakeLocks) != 1 || result.WakeLocks[0].Status != test.wantStatus {
				t.Fatalf("replacement status = %#v, want %q", result.WakeLocks, test.wantStatus)
			}
			if result.WakeLocks[0].NotifierAbsent {
				t.Fatalf("replacement classified absent: %#v", result.WakeLocks[0])
			}
			if _, found := findOpsHint(result.Hints, "unread_backlog_no_notifier"); found {
				t.Fatalf("replacement emitted no-notifier hint: %#v", result.Hints)
			}
		})
	}
}
