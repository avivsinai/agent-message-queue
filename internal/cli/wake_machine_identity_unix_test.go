//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNewWakeLockRecordsMachineIDWhenAvailable(t *testing.T) {
	root := secureTempDirForTest(t)
	stubCurrentWakeMachineID(t, testMachineUUID)

	lock, err := newWakeLock(root, "codex", wakeLockAcquireOptions{wakeMode: wakeInjectModeRaw})
	if err != nil {
		t.Fatalf("new wake lock: %v", err)
	}
	if lock.MachineID != testMachineUUID {
		t.Fatalf("machine id = %q, want %q", lock.MachineID, testMachineUUID)
	}
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"machine_id":"`+testMachineUUID+`"`)) {
		t.Fatalf("lock JSON lacks machine_id: %s", data)
	}
}

// A host without a stable machine identity is a valid runtime state: lock
// creation must succeed and the omitempty field must stay off the wire.
func TestNewWakeLockOmitsMachineIDWhenUnavailable(t *testing.T) {
	root := secureTempDirForTest(t)
	stubCurrentWakeMachineID(t, "")

	lock, err := newWakeLock(root, "codex", wakeLockAcquireOptions{wakeMode: wakeInjectModeRaw})
	if err != nil {
		t.Fatalf("new wake lock without machine id: %v", err)
	}
	if lock.MachineID != "" {
		t.Fatalf("machine id = %q, want empty", lock.MachineID)
	}
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"machine_id"`)) {
		t.Fatalf("lock JSON contains machine_id despite unavailable identity: %s", data)
	}
}
