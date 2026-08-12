package cli

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

const (
	testMachineUUID  = "11111111-2222-3333-4444-555555555555"
	otherMachineUUID = "99999999-8888-7777-6666-555555555555"
	testBootUUID     = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	otherBootUUID    = "FFFFFFFF-0000-1111-2222-333333333333"
	legacyBoottimeID = "1781686509.734623000"
)

func stubCurrentWakeMachineID(t *testing.T, id string) {
	t.Helper()
	old := currentWakeMachineID
	currentWakeMachineID = func() string { return id }
	t.Cleanup(func() { currentWakeMachineID = old })
}

func stubCurrentWakeBootID(t *testing.T, id string) {
	t.Helper()
	old := currentWakeBootID
	currentWakeBootID = func() string { return id }
	t.Cleanup(func() { currentWakeBootID = old })
}

func stubCurrentWakeHostname(t *testing.T, fn func() (string, error)) {
	t.Helper()
	old := currentWakeHostname
	currentWakeHostname = fn
	t.Cleanup(func() { currentWakeHostname = old })
}

func staticHostname(name string) func() (string, error) {
	return func() (string, error) { return name, nil }
}

func TestClassifyWakeLockMachine(t *testing.T) {
	tests := []struct {
		name       string
		lock       wakeLock
		machineID  string
		bootID     string
		hostname   func() (string, error)
		wantState  wakeMachineComparison
		wantReason string
	}{
		{
			name:      "machine id match outranks hostname drift",
			lock:      wakeLock{MachineID: testMachineUUID, Hostname: "mac", BootID: legacyBoottimeID},
			machineID: testMachineUUID,
			bootID:    testBootUUID,
			hostname:  staticHostname("avivs-macbook-pro.local"),
			wantState: wakeMachineSame,
		},
		{
			name:      "machine id comparison is case insensitive",
			lock:      wakeLock{MachineID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Hostname: "mac"},
			machineID: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
			hostname:  staticHostname("avivs-macbook-pro.local"),
			wantState: wakeMachineSame,
		},
		{
			name:       "machine id mismatch outranks hostname match",
			lock:       wakeLock{MachineID: otherMachineUUID, Hostname: "host-a"},
			machineID:  testMachineUUID,
			hostname:   staticHostname("host-a"),
			wantState:  wakeMachineDifferent,
			wantReason: "machine id mismatch",
		},
		{
			name:      "same boot bridge proves same machine without machine id",
			lock:      wakeLock{Hostname: "mac", BootID: testBootUUID},
			bootID:    testBootUUID,
			hostname:  staticHostname("avivs-macbook-pro.local"),
			wantState: wakeMachineSame,
		},
		{
			name:       "bridge requires uuid boot identity on both sides",
			lock:       wakeLock{Hostname: "mac", BootID: legacyBoottimeID},
			bootID:     testBootUUID,
			hostname:   staticHostname("avivs-macbook-pro.local"),
			wantState:  wakeMachineUnknown,
			wantReason: "hostname mismatch",
		},
		{
			name:       "cross boot with hostname drift stays unknown",
			lock:       wakeLock{Hostname: "mac", BootID: otherBootUUID},
			bootID:     testBootUUID,
			hostname:   staticHostname("avivs-macbook-pro.local"),
			wantState:  wakeMachineUnknown,
			wantReason: "hostname mismatch",
		},
		{
			name:      "cross boot with matching hostname stays same",
			lock:      wakeLock{Hostname: "host-a", BootID: otherBootUUID},
			bootID:    testBootUUID,
			hostname:  staticHostname("host-a"),
			wantState: wakeMachineSame,
		},
		{
			name:      "recorded machine id with unavailable current falls back to hostname",
			lock:      wakeLock{MachineID: testMachineUUID, Hostname: "host-a"},
			machineID: "",
			hostname:  staticHostname("host-a"),
			wantState: wakeMachineSame,
		},
		{
			name:       "recorded machine id with unavailable current still fails on hostname drift",
			lock:       wakeLock{MachineID: testMachineUUID, Hostname: "mac"},
			machineID:  "",
			hostname:   staticHostname("avivs-macbook-pro.local"),
			wantState:  wakeMachineUnknown,
			wantReason: "hostname mismatch",
		},
		{
			name:       "empty hostname without error is unknown",
			lock:       wakeLock{Hostname: "host-a"},
			hostname:   staticHostname(""),
			wantState:  wakeMachineUnknown,
			wantReason: "hostname unavailable",
		},
		{
			name:      "legacy lock without hostname skips the gate",
			lock:      wakeLock{},
			machineID: testMachineUUID,
			hostname:  staticHostname("host-a"),
			wantState: wakeMachineSame,
		},
		{
			name:       "hostname unavailable is unknown",
			lock:       wakeLock{Hostname: "host-a"},
			hostname:   func() (string, error) { return "", errors.New("no hostname") },
			wantState:  wakeMachineUnknown,
			wantReason: "hostname unavailable: no hostname",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubCurrentWakeMachineID(t, tt.machineID)
			stubCurrentWakeBootID(t, tt.bootID)
			stubCurrentWakeHostname(t, tt.hostname)
			state, reason := classifyWakeLockMachine(tt.lock)
			if state != tt.wantState || reason != tt.wantReason {
				t.Fatalf("state = %v reason = %q, want %v %q", state, reason, tt.wantState, tt.wantReason)
			}
		})
	}
}

// A dead wake from an old boot of this same machine must classify stale, not
// unverified, even when the network-derived hostname drifted after the lock
// was written. This is the resume-after-reboot scenario that used to force an
// interactive prompt on every coop exec.
func TestInspectWakeLockHostnameDriftOnSameMachineDeadPIDIsStale(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          66121,
		TTY:          "ttys001",
		Hostname:     "definitely-not-this-host",
		MachineID:    testMachineUUID,
		ProcessStart: "1783283179.960200000",
		BootID:       legacyBoottimeID,
		Executable:   "amq",
	})
	stubCurrentWakeMachineID(t, testMachineUUID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockStale {
		t.Fatalf("inspection status = %q (reason %q), want stale", inspection.Status, inspection.Reason)
	}
}

// A lock recording a different stable machine identity must stay unverified
// even when the hostname happens to match: hostname is the weaker signal.
func TestInspectWakeLockForeignMachineIDIsUnverified(t *testing.T) {
	root := secureTempDirForTest(t)
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Fatalf("os.Hostname: %q, %v", hostname, err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          66121,
		TTY:          "ttys001",
		Hostname:     hostname,
		MachineID:    otherMachineUUID,
		ProcessStart: "start-1",
		Executable:   "amq",
	})
	stubCurrentWakeMachineID(t, testMachineUUID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockUnverified {
		t.Fatalf("inspection status = %q (reason %q), want unverified", inspection.Status, inspection.Reason)
	}
	if inspection.Reason != "machine id mismatch" {
		t.Fatalf("inspection reason = %q, want machine id mismatch", inspection.Reason)
	}
}

// A null machine_id joins the known-field trust matrix: the lock is rejected
// as malformed and stays unverified rather than proving anything.
func TestInspectWakeLockNullMachineIDIsRejectedAsMalformed(t *testing.T) {
	root := secureTempDirForTest(t)
	path := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, TTY: "ttys001"})
	raw, err := json.Marshal(map[string]any{
		"pid":        66121,
		"tty":        "ttys001",
		"root":       canonicalWakeRoot(root),
		"agent":      "codex",
		"started":    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"hostname":   "definitely-not-this-host",
		"machine_id": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stubCurrentWakeMachineID(t, testMachineUUID)

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockUnverified {
		t.Fatalf("inspection status = %q (reason %q), want unverified", inspection.Status, inspection.Reason)
	}
	if inspection.Reason != `wake lock JSON field "machine_id" must not be null` {
		t.Fatalf("inspection reason = %q, want null machine_id rejection", inspection.Reason)
	}
}

// Locks that predate machine_id keep today's fail-closed behavior: hostname
// drift without stronger same-machine evidence stays unverified.
func TestInspectWakeLockLegacyHostnameDriftStaysUnverified(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          66121,
		TTY:          "ttys001",
		Hostname:     "definitely-not-this-host",
		ProcessStart: "1783283179.960200000",
		BootID:       legacyBoottimeID,
		Executable:   "amq",
	})
	stubCurrentWakeMachineID(t, testMachineUUID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockUnverified {
		t.Fatalf("inspection status = %q (reason %q), want unverified", inspection.Status, inspection.Reason)
	}
	if inspection.Reason != "hostname mismatch" {
		t.Fatalf("inspection reason = %q, want hostname mismatch", inspection.Reason)
	}
}
