//go:build darwin || linux

package cli

type wakeResumeDisposition uint8

const (
	wakeResumeProceed wakeResumeDisposition = iota
	wakeResumeDefer
	wakeResumeRefuse
)

type wakeResumeLifecycle uint8

const (
	wakeResumeLifecycleStarting wakeResumeLifecycle = iota
	wakeResumeLifecycleAdmitted
)

type wakeResumeScan uint8

const (
	wakeResumeScanTransientFailure wakeResumeScan = iota
	wakeResumeScanComplete
)

type wakeResumeAuthorityObservation uint8

const (
	// The zero value is inconclusive so a partially populated proof cannot proceed.
	wakeResumeAuthorityInconclusive wakeResumeAuthorityObservation = iota
	wakeResumeAuthorityExact
	wakeResumeAuthorityLost
)

const (
	wakeResumeReasonNotAdmitted           = "wake_not_admitted"
	wakeResumeReasonInputDelivery         = "input_delivery_in_progress"
	wakeResumeReasonInputProgress         = "input_progress_confirmed"
	wakeResumeReasonInputUncertain        = "input_acceptance_uncertain"
	wakeResumeReasonInputRecovery         = "input_recovery_required"
	wakeResumeReasonRepairState           = "repair_state_active"
	wakeResumeReasonArbitraryInjector     = "arbitrary_inject_command"
	wakeResumeReasonDestructiveInterrupt  = "destructive_interrupt"
	wakeResumeReasonInjectorActive        = "external_injector_active"
	wakeResumeReasonInjectorCleanup       = "injector_cleanup_pending"
	wakeResumeReasonTerminalRetry         = "terminal_suffix_retry_pending"
	wakeResumeReasonScanRetry             = "inbox_scan_retry_pending"
	wakeResumeReasonWatcherUnarmed        = "watcher_unarmed"
	wakeResumeReasonWatcherError          = "watcher_error"
	wakeResumeReasonWatcherRebind         = "watcher_rebind_pending"
	wakeResumeReasonGenerationRecovery    = "unreadable_generation_recovery"
	wakeResumeReasonDebounceActive        = "debounce_callback_active"
	wakeResumeReasonControlHandlers       = "control_handlers_active"
	wakeResumeReasonControlListener       = "control_listener_unavailable"
	wakeResumeReasonDirectoriesUnverified = "wake_directories_unverified"
	wakeResumeReasonFinalScanIncomplete   = "final_inbox_scan_incomplete"
)

const (
	wakeResumeReasonOwnerInconclusive            = "owner_observation_inconclusive"
	wakeResumeReasonOwnerLost                    = "owner_identity_lost"
	wakeResumeReasonTerminalIdentityInconclusive = "terminal_identity_inconclusive"
	wakeResumeReasonTerminalIdentityLost         = "terminal_identity_lost"
	wakeResumeReasonGenerationInconclusive       = "wake_generation_inconclusive"
	wakeResumeReasonGenerationLost               = "wake_generation_lost"
	wakeResumeReasonLockTargetInconclusive       = "wake_lock_target_inconclusive"
	wakeResumeReasonLockTargetLost               = "wake_lock_target_lost"
)

type wakeResumeQuiescence struct {
	Lifecycle wakeResumeLifecycle
	Delivery  wakeInputDeliveryState

	RecoveryRequired      bool
	RepairLineage         bool
	RepairHandoff         bool
	RepairPrepared        bool
	RepairFloorTransition bool
	BaselineInherited     bool

	InjectViaActive        bool
	InjectorCleanupPending bool
	ArbitraryInjectCmd     bool
	DestructiveInterrupt   bool

	TerminalSuffixRetry          bool
	ScanRetry                    bool
	WatcherArmed                 bool
	WatcherError                 bool
	WatcherRebindRetry           bool
	UnreadableGenerationRecovery bool

	DebounceExecuting           bool
	ControlHandlers             uint
	ControlListenerReady        bool
	OwnerObservation            wakeResumeAuthorityObservation
	TerminalIdentityObservation wakeResumeAuthorityObservation
	GenerationObservation       wakeResumeAuthorityObservation
	LockTargetObservation       wakeResumeAuthorityObservation
	CanonicalDirs               bool
	FinalScan                   wakeResumeScan

	// These fields are deliberately not gates. Durable unread work and old
	// doorbell/backoff state become an immediately-due fresh cohort after resume.
	FinalScanMessages int
	PendingDoorbell   bool
}

type wakeResumeQuiescenceDecision struct {
	Disposition wakeResumeDisposition
	Reason      string
}

func classifyWakeResumeQuiescence(state wakeResumeQuiescence) wakeResumeQuiescenceDecision {
	refuse := func(reason string) wakeResumeQuiescenceDecision {
		return wakeResumeQuiescenceDecision{Disposition: wakeResumeRefuse, Reason: reason}
	}
	deferReload := func(reason string) wakeResumeQuiescenceDecision {
		return wakeResumeQuiescenceDecision{Disposition: wakeResumeDefer, Reason: reason}
	}

	switch {
	case state.Lifecycle != wakeResumeLifecycleAdmitted:
		return refuse(wakeResumeReasonNotAdmitted)
	case state.RecoveryRequired:
		return refuse(wakeResumeReasonInputRecovery)
	case state.RepairLineage || state.RepairHandoff || state.RepairPrepared ||
		state.RepairFloorTransition || state.BaselineInherited:
		return refuse(wakeResumeReasonRepairState)
	case state.ArbitraryInjectCmd:
		return refuse(wakeResumeReasonArbitraryInjector)
	case state.DestructiveInterrupt:
		return refuse(wakeResumeReasonDestructiveInterrupt)
	case state.Delivery.acceptanceUncertain:
		return refuse(wakeResumeReasonInputUncertain)
	case state.Delivery.acceptedBytes > 0:
		return refuse(wakeResumeReasonInputProgress)
	case state.Delivery.phase != wakeInputDeliveryIdle:
		return refuse(wakeResumeReasonInputDelivery)
	case state.OwnerObservation == wakeResumeAuthorityLost:
		return refuse(wakeResumeReasonOwnerLost)
	case state.TerminalIdentityObservation == wakeResumeAuthorityLost:
		return refuse(wakeResumeReasonTerminalIdentityLost)
	case state.GenerationObservation == wakeResumeAuthorityLost:
		return refuse(wakeResumeReasonGenerationLost)
	case state.LockTargetObservation == wakeResumeAuthorityLost:
		return refuse(wakeResumeReasonLockTargetLost)
	case state.InjectViaActive:
		return deferReload(wakeResumeReasonInjectorActive)
	case state.InjectorCleanupPending:
		return deferReload(wakeResumeReasonInjectorCleanup)
	case state.TerminalSuffixRetry:
		return deferReload(wakeResumeReasonTerminalRetry)
	case state.ScanRetry:
		return deferReload(wakeResumeReasonScanRetry)
	case !state.WatcherArmed:
		return deferReload(wakeResumeReasonWatcherUnarmed)
	case state.WatcherError:
		return deferReload(wakeResumeReasonWatcherError)
	case state.WatcherRebindRetry:
		return deferReload(wakeResumeReasonWatcherRebind)
	case state.UnreadableGenerationRecovery:
		return deferReload(wakeResumeReasonGenerationRecovery)
	case state.DebounceExecuting:
		return deferReload(wakeResumeReasonDebounceActive)
	case state.ControlHandlers > 0:
		return deferReload(wakeResumeReasonControlHandlers)
	case !state.ControlListenerReady:
		return deferReload(wakeResumeReasonControlListener)
	case state.OwnerObservation != wakeResumeAuthorityExact:
		return deferReload(wakeResumeReasonOwnerInconclusive)
	case state.TerminalIdentityObservation != wakeResumeAuthorityExact:
		return deferReload(wakeResumeReasonTerminalIdentityInconclusive)
	case state.GenerationObservation != wakeResumeAuthorityExact:
		return deferReload(wakeResumeReasonGenerationInconclusive)
	case state.LockTargetObservation != wakeResumeAuthorityExact:
		return deferReload(wakeResumeReasonLockTargetInconclusive)
	case !state.CanonicalDirs:
		return deferReload(wakeResumeReasonDirectoriesUnverified)
	case state.FinalScan != wakeResumeScanComplete:
		return deferReload(wakeResumeReasonFinalScanIncomplete)
	default:
		return wakeResumeQuiescenceDecision{Disposition: wakeResumeProceed}
	}
}

func wakeInputDeliveryPhaseName(phase wakeInputDeliveryPhase) string {
	switch phase {
	case wakeInputDeliveryIdle:
		return "idle"
	case wakeInputPayloadPending:
		return "payload_pending"
	case wakeInputRawPreludePending:
		return "raw_prelude_pending"
	case wakeInputPrimarySubmitPending:
		return "primary_submit_pending"
	case wakeInputRawFirstSubmitQueued:
		return "raw_first_submit_queued"
	case wakeInputRawRescuePending:
		return "raw_rescue_pending"
	case wakeInputRawRescueQueued:
		return "raw_rescue_queued"
	default:
		return "unknown"
	}
}
