//go:build darwin

package cli

import "testing"

func TestLiveRawOrphanState(t *testing.T) {
	i := wakeLockInspection{IdentityConfirmed: true, Process: wakeProcessInfo{Running: true}, Lock: wakeLock{WakeMode: "raw"}}
	if !isLiveRawOrphan(i) {
		t.Fatal("expected live raw orphan")
	}
	i.Process.Running = false
	if isLiveRawOrphan(i) {
		t.Fatal("dead process is not a live raw orphan")
	}
}
