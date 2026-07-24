package cli

import (
	"fmt"
	"strings"
)

type wakeClaimClass uint8

const (
	wakeClaimAbsent wakeClaimClass = iota
	wakeClaimGeneric
	wakeClaimAuthoritative
	wakeClaimInvalid
)

type wakeOwnerTransitionRequest uint8

const (
	wakeGenericRequestAcquire wakeOwnerTransitionRequest = iota
	wakeGenericRequestMutate
	wakeOwnerRequestAcquire
	wakeOwnerRequestRecover
)

type wakeOwnerTransitionAction uint8

const (
	wakeOwnerActionRefuse wakeOwnerTransitionAction = iota
	wakeOwnerActionLegacy
	wakeOwnerActionPublish
	wakeOwnerActionReuse
	wakeOwnerActionRelease
	wakeOwnerActionStopAndRelease
	wakeOwnerActionCleanOrphan
)

func (action wakeOwnerTransitionAction) String() string {
	switch action {
	case wakeOwnerActionLegacy:
		return "legacy"
	case wakeOwnerActionPublish:
		return "publish"
	case wakeOwnerActionReuse:
		return "reuse"
	case wakeOwnerActionRelease:
		return "release"
	case wakeOwnerActionStopAndRelease:
		return "stop and release"
	case wakeOwnerActionCleanOrphan:
		return "clean orphan"
	default:
		return "refuse"
	}
}

type wakeOwnerTransitionEvidence struct {
	Request            wakeOwnerTransitionRequest
	Claim              wakeClaimClass
	RequestedState     wakeOwnerIdentityState
	PersistedState     wakeOwnerIdentityState
	OwnersEqual        bool
	WakeUsable         bool
	WakeExactStoppable bool
	WakeAbsent         bool
	Authenticated      bool
}

// decideOwnerLifecycleTransition is the policy core shared by acquisition,
// recovery, and mutation gates. Callers gather fresh evidence while holding the
// per-handle lifecycle guard; this function deliberately performs no I/O.
func decideOwnerLifecycleTransition(e wakeOwnerTransitionEvidence) wakeOwnerTransitionAction {
	switch e.Request {
	case wakeGenericRequestAcquire, wakeGenericRequestMutate:
		if e.Claim == wakeClaimAbsent || e.Claim == wakeClaimGeneric {
			return wakeOwnerActionLegacy
		}
		return wakeOwnerActionRefuse

	case wakeOwnerRequestAcquire:
		if e.RequestedState != wakeOwnerSame {
			return wakeOwnerActionRefuse
		}
		switch e.Claim {
		case wakeClaimAbsent:
			return wakeOwnerActionPublish
		case wakeClaimAuthoritative:
			switch e.PersistedState {
			case wakeOwnerSame:
				if e.OwnersEqual && e.WakeUsable {
					return wakeOwnerActionReuse
				}
				return wakeOwnerActionRefuse
			case wakeOwnerDead:
				return ownerReleaseAction(e)
			default:
				return wakeOwnerActionRefuse
			}
		default:
			return wakeOwnerActionRefuse
		}

	case wakeOwnerRequestRecover:
		switch e.Claim {
		case wakeClaimAbsent:
			return wakeOwnerActionCleanOrphan
		case wakeClaimAuthoritative:
			if e.PersistedState == wakeOwnerDead ||
				(e.PersistedState == wakeOwnerSame && e.Authenticated) {
				return ownerReleaseAction(e)
			}
		}
	}
	return wakeOwnerActionRefuse
}

func validateGenericWakeLifecycleTransition(
	inspection wakeLockInspection,
	request wakeOwnerTransitionRequest,
) error {
	claim := classifyWakeClaimForGenericTransition(inspection)
	action := decideOwnerLifecycleTransition(wakeOwnerTransitionEvidence{
		Request: request,
		Claim:   claim,
	})
	if action == wakeOwnerActionLegacy {
		return nil
	}
	operation := "mutation"
	if request == wakeGenericRequestAcquire {
		operation = "acquisition"
	}
	if claim == wakeClaimAuthoritative {
		return fmt.Errorf(
			"owner-bound wake claims require 'amq wake recover-owner --me %s'",
			inspection.Agent,
		)
	}
	reason := strings.TrimSpace(inspection.Reason)
	if reason == "" {
		reason = "persisted wake claim is not a valid ownerless generation"
	}
	return fmt.Errorf(
		"wake state for %s is unverified; refusing generic %s: %s",
		inspection.Agent,
		operation,
		reason,
	)
}

func ownerReleaseAction(e wakeOwnerTransitionEvidence) wakeOwnerTransitionAction {
	if e.WakeExactStoppable {
		return wakeOwnerActionStopAndRelease
	}
	if e.WakeAbsent {
		return wakeOwnerActionRelease
	}
	return wakeOwnerActionRefuse
}
