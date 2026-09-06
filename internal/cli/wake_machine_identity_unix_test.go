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
