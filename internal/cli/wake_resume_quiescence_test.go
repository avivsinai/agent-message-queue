//go:build darwin || linux

package cli

import (
	"testing"
)

func readyWakeResumeQuiescenceForTest() wakeResumeQuiescence {
	return wakeResumeQuiescence{
		Lifecycle:                   wakeResumeLifecycleAdmitted,
		WatcherArmed:                true,
		ControlListenerReady:        true,
		OwnerObservation:            wakeResumeAuthorityExact,
		TerminalIdentityObservation: wakeResumeAuthorityExact,
		GenerationObservation:       wakeResumeAuthorityExact,
		LockTargetObservation:       wakeResumeAuthorityExact,
		CanonicalDirs:               true,
		FinalScan:                   wakeResumeScanComplete,
		FinalScanMessages:           3,
		PendingDoorbell:             true,
	}
}

func TestWakeResumeQuiescenceAllowsUnreadCohortAndExistingBackoff(t *testing.T) {
	state := readyWakeResumeQuiescenceForTest()
	state.FinalScanMessages = 7
	state.PendingDoorbell = true

	decision := classifyWakeResumeQuiescence(state)

	if decision.Disposition != wakeResumeProceed || decision.Reason != "" {
		t.Fatalf("decision = %#v, want proceed", decision)
	}
}
