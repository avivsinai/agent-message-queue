//go:build darwin

package cli

import "testing"

func TestLiveRawOrphanState(t *testing.T) {
	i := wakeLockInspection{IdentityConfirmed: true, Process: wakeProcessInfo{Running: true}, Lock: wakeLock{WakeMode: "raw"}}
	if !isLiveRawOrphan(i) {
		t.Fatal("expected live raw orphan")
	}
	i.Lock.OwnerSchema = wakeOwnerLockSchema
	i.Lock.Owner = &wakeOwner{
		PID:          42,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    7,
	}
	if isLiveRawOrphan(i) {
		t.Fatal("owner-bound wake is not a live raw orphan")
	}
	i.Lock.OwnerSchema = 0
	i.Lock.Owner = nil
	i.Process.Running = false
	if isLiveRawOrphan(i) {
		t.Fatal("dead process is not a live raw orphan")
	}
}
