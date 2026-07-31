//go:build darwin || linux

package cli

import (
	"reflect"
	"testing"
)

func readyWakeResumeQuiescenceForTest() wakeResumeQuiescence {
	return wakeResumeQuiescence{
		Lifecycle:            wakeResumeLifecycleAdmitted,
		WatcherArmed:         true,
		ControlListenerReady: true,
		OwnerLive:            true,
		AuthorityExact:       true,
		CanonicalDirs:        true,
		FinalScan:            wakeResumeScanComplete,
		FinalScanMessages:    3,
		PendingDoorbell:      true,
	}
}

func TestWakeResumeQuiescenceRefusesUntransferableStateWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*wakeResumeQuiescence)
		reason string
	}{
		{
			name: "startup not admitted",
			mutate: func(state *wakeResumeQuiescence) {
				state.Lifecycle = wakeResumeLifecycleStarting
			},
			reason: wakeResumeReasonNotAdmitted,
		},
		{
			name: "input recovery required",
			mutate: func(state *wakeResumeQuiescence) {
				state.RecoveryRequired = true
			},
			reason: wakeResumeReasonInputRecovery,
		},
		{
			name: "repair lineage",
			mutate: func(state *wakeResumeQuiescence) {
				state.RepairLineage = true
			},
			reason: wakeResumeReasonRepairState,
		},
		{
			name: "repair handoff",
			mutate: func(state *wakeResumeQuiescence) {
				state.RepairHandoff = true
			},
			reason: wakeResumeReasonRepairState,
		},
		{
			name: "repair prepared",
			mutate: func(state *wakeResumeQuiescence) {
				state.RepairPrepared = true
			},
			reason: wakeResumeReasonRepairState,
		},
		{
			name: "repair floor transition",
			mutate: func(state *wakeResumeQuiescence) {
				state.RepairFloorTransition = true
			},
			reason: wakeResumeReasonRepairState,
		},
		{
			name: "inherited repair baseline",
			mutate: func(state *wakeResumeQuiescence) {
				state.BaselineInherited = true
			},
			reason: wakeResumeReasonRepairState,
		},
		{
			name: "arbitrary inject command",
			mutate: func(state *wakeResumeQuiescence) {
				state.ArbitraryInjectCmd = true
			},
			reason: wakeResumeReasonArbitraryInjector,
		},
		{
			name: "destructive interrupt",
			mutate: func(state *wakeResumeQuiescence) {
				state.DestructiveInterrupt = true
			},
			reason: wakeResumeReasonDestructiveInterrupt,
		},
	}

	for phase := wakeInputPayloadPending; phase <= wakeInputRawRescueQueued; phase++ {
		phase := phase
		tests = append(tests, struct {
			name   string
			mutate func(*wakeResumeQuiescence)
			reason string
		}{
			name: "input phase " + wakeInputDeliveryPhaseName(phase),
			mutate: func(state *wakeResumeQuiescence) {
				state.Delivery.phase = phase
			},
			reason: wakeResumeReasonInputDelivery,
		})
	}
	tests = append(tests,
		struct {
			name   string
			mutate func(*wakeResumeQuiescence)
			reason string
		}{
			name: "accepted input bytes",
			mutate: func(state *wakeResumeQuiescence) {
				state.Delivery.acceptedBytes = 1
			},
			reason: wakeResumeReasonInputProgress,
		},
		struct {
			name   string
			mutate func(*wakeResumeQuiescence)
			reason string
		}{
			name: "uncertain input acceptance",
			mutate: func(state *wakeResumeQuiescence) {
				state.Delivery.acceptanceUncertain = true
			},
			reason: wakeResumeReasonInputUncertain,
		},
	)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := readyWakeResumeQuiescenceForTest()
			test.mutate(&state)
			before := state

			decision := classifyWakeResumeQuiescence(state)

			if decision.Disposition != wakeResumeRefuse || decision.Reason != test.reason {
				t.Fatalf("decision = %#v, want refuse reason %q", decision, test.reason)
			}
			if !reflect.DeepEqual(state, before) {
				t.Fatalf("classifier mutated input:\nbefore=%#v\nafter=%#v", before, state)
			}
		})
	}
}

func TestWakeResumeQuiescenceDefersTransientStateWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*wakeResumeQuiescence)
		reason string
	}{
		{name: "injector active", mutate: func(s *wakeResumeQuiescence) { s.InjectViaActive = true }, reason: wakeResumeReasonInjectorActive},
		{name: "injector cleanup", mutate: func(s *wakeResumeQuiescence) { s.InjectorCleanupPending = true }, reason: wakeResumeReasonInjectorCleanup},
		{name: "terminal suffix retry", mutate: func(s *wakeResumeQuiescence) { s.TerminalSuffixRetry = true }, reason: wakeResumeReasonTerminalRetry},
		{name: "scan retry", mutate: func(s *wakeResumeQuiescence) { s.ScanRetry = true }, reason: wakeResumeReasonScanRetry},
		{name: "watcher unarmed", mutate: func(s *wakeResumeQuiescence) { s.WatcherArmed = false }, reason: wakeResumeReasonWatcherUnarmed},
		{name: "watcher error", mutate: func(s *wakeResumeQuiescence) { s.WatcherError = true }, reason: wakeResumeReasonWatcherError},
		{name: "watcher rebind", mutate: func(s *wakeResumeQuiescence) { s.WatcherRebindRetry = true }, reason: wakeResumeReasonWatcherRebind},
		{name: "unreadable generation recovery", mutate: func(s *wakeResumeQuiescence) { s.UnreadableGenerationRecovery = true }, reason: wakeResumeReasonGenerationRecovery},
		{name: "debounce executing", mutate: func(s *wakeResumeQuiescence) { s.DebounceExecuting = true }, reason: wakeResumeReasonDebounceActive},
		{name: "one control handler", mutate: func(s *wakeResumeQuiescence) { s.ControlHandlers = 1 }, reason: wakeResumeReasonControlHandlers},
		{name: "two control handlers", mutate: func(s *wakeResumeQuiescence) { s.ControlHandlers = 2 }, reason: wakeResumeReasonControlHandlers},
		{name: "control listener unavailable", mutate: func(s *wakeResumeQuiescence) { s.ControlListenerReady = false }, reason: wakeResumeReasonControlListener},
		{name: "owner unavailable", mutate: func(s *wakeResumeQuiescence) { s.OwnerLive = false }, reason: wakeResumeReasonOwnerUnavailable},
		{name: "authority drift", mutate: func(s *wakeResumeQuiescence) { s.AuthorityExact = false }, reason: wakeResumeReasonAuthorityUnverified},
		{name: "directories unverified", mutate: func(s *wakeResumeQuiescence) { s.CanonicalDirs = false }, reason: wakeResumeReasonDirectoriesUnverified},
		{name: "full scan incomplete", mutate: func(s *wakeResumeQuiescence) { s.FinalScan = wakeResumeScanTransientFailure }, reason: wakeResumeReasonFinalScanIncomplete},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := readyWakeResumeQuiescenceForTest()
			test.mutate(&state)
			before := state

			decision := classifyWakeResumeQuiescence(state)

			if decision.Disposition != wakeResumeDefer || decision.Reason != test.reason {
				t.Fatalf("decision = %#v, want defer reason %q", decision, test.reason)
			}
			if !reflect.DeepEqual(state, before) {
				t.Fatalf("classifier mutated input:\nbefore=%#v\nafter=%#v", before, state)
			}
		})
	}
}

func TestWakeResumeQuiescenceAllowsUnreadCohortAndExistingBackoff(t *testing.T) {
	state := readyWakeResumeQuiescenceForTest()
	state.FinalScanMessages = 7
	state.PendingDoorbell = true

	decision := classifyWakeResumeQuiescence(state)

	if decision.Disposition != wakeResumeReady || decision.Reason != "" {
		t.Fatalf("decision = %#v, want ready", decision)
	}
}
