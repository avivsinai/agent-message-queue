//go:build darwin || linux

package cli

import (
	"reflect"
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

func TestWakeResumeAuthorityObservationZeroIsInconclusive(t *testing.T) {
	var observation wakeResumeAuthorityObservation
	if observation != wakeResumeAuthorityInconclusive {
		t.Fatalf("zero observation = %v, want inconclusive", observation)
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
		{
			name: "owner identity lost",
			mutate: func(state *wakeResumeQuiescence) {
				state.OwnerObservation = wakeResumeAuthorityLost
			},
			reason: wakeResumeReasonOwnerLost,
		},
		{
			name: "terminal identity lost",
			mutate: func(state *wakeResumeQuiescence) {
				state.TerminalIdentityObservation = wakeResumeAuthorityLost
			},
			reason: wakeResumeReasonTerminalIdentityLost,
		},
		{
			name: "generation lost",
			mutate: func(state *wakeResumeQuiescence) {
				state.GenerationObservation = wakeResumeAuthorityLost
			},
			reason: wakeResumeReasonGenerationLost,
		},
		{
			name: "lock or target lost",
			mutate: func(state *wakeResumeQuiescence) {
				state.LockTargetObservation = wakeResumeAuthorityLost
			},
			reason: wakeResumeReasonLockTargetLost,
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
		{name: "owner observation inconclusive", mutate: func(s *wakeResumeQuiescence) { s.OwnerObservation = wakeResumeAuthorityInconclusive }, reason: wakeResumeReasonOwnerInconclusive},
		{name: "terminal identity inconclusive", mutate: func(s *wakeResumeQuiescence) { s.TerminalIdentityObservation = wakeResumeAuthorityInconclusive }, reason: wakeResumeReasonTerminalIdentityInconclusive},
		{name: "generation inconclusive", mutate: func(s *wakeResumeQuiescence) { s.GenerationObservation = wakeResumeAuthorityInconclusive }, reason: wakeResumeReasonGenerationInconclusive},
		{name: "lock or target inconclusive", mutate: func(s *wakeResumeQuiescence) { s.LockTargetObservation = wakeResumeAuthorityInconclusive }, reason: wakeResumeReasonLockTargetInconclusive},
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

	if decision.Disposition != wakeResumeProceed || decision.Reason != "" {
		t.Fatalf("decision = %#v, want proceed", decision)
	}
}

func TestWakeResumeQuiescencePreservesRefuseAndDeferPrecedence(t *testing.T) {
	t.Run("existing refusal precedes authority loss", func(t *testing.T) {
		state := readyWakeResumeQuiescenceForTest()
		state.RecoveryRequired = true
		state.OwnerObservation = wakeResumeAuthorityLost

		decision := classifyWakeResumeQuiescence(state)

		if decision.Disposition != wakeResumeRefuse || decision.Reason != wakeResumeReasonInputRecovery {
			t.Fatalf("decision = %#v, want input-recovery refusal", decision)
		}
	})

	t.Run("authority loss precedes transient deferral", func(t *testing.T) {
		state := readyWakeResumeQuiescenceForTest()
		state.OwnerObservation = wakeResumeAuthorityLost
		state.InjectViaActive = true

		decision := classifyWakeResumeQuiescence(state)

		if decision.Disposition != wakeResumeRefuse || decision.Reason != wakeResumeReasonOwnerLost {
			t.Fatalf("decision = %#v, want owner-loss refusal", decision)
		}
	})

	t.Run("existing transient deferral precedes inconclusive authority", func(t *testing.T) {
		state := readyWakeResumeQuiescenceForTest()
		state.InjectViaActive = true
		state.OwnerObservation = wakeResumeAuthorityInconclusive

		decision := classifyWakeResumeQuiescence(state)

		if decision.Disposition != wakeResumeDefer || decision.Reason != wakeResumeReasonInjectorActive {
			t.Fatalf("decision = %#v, want injector-active deferral", decision)
		}
	})
}
