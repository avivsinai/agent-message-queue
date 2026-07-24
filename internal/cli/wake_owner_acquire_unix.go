//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

func classifyPersistedWakeClaim(inspection wakeLockInspection) wakeClaimClass {
	if !inspection.Exists {
		return wakeClaimAbsent
	}
	if inspection.fileInfo == nil {
		return wakeClaimInvalid
	}
	switch inspection.fileInfo.Mode().Perm() {
	case wakeOwnerLockFileMode:
		if validateAuthoritativeWakeLockEnvelope(
			inspection.Lock,
			inspection.Root,
			inspection.Agent,
		) != nil {
			return wakeClaimInvalid
		}
		return wakeClaimAuthoritative
	case 0o600:
		if inspection.Lock.OwnerSchema != 0 ||
			inspection.Lock.Owner != nil ||
			inspection.Lock.WakeMode == wakeOwnerWakeMode {
			return wakeClaimInvalid
		}
		return wakeClaimGeneric
	default:
		return wakeClaimInvalid
	}
}

func validateAuthoritativeWakeClaimPairAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (wakeTarget, error) {
	if classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
		return wakeTarget{}, fmt.Errorf("wake lock is not an authoritative owner claim")
	}
	return readAuthoritativeWakeTargetAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
		inspection.Lock,
	)
}

func authoritativeWakeRecoveryTargetAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (*wakeTarget, error) {
	target, exists, err := readWakeTargetRawAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
	)
	if err != nil {
		// The authoritative lock retains owner evidence. An unreadable or
		// corrupt target is preserved as an orphan rather than becoming a
		// permanent barrier to authenticated recovery.
		return nil, nil
	}
	if !exists {
		return nil, nil
	}
	if target.Owner == nil {
		return nil, nil
	}
	if err := validateAuthoritativeWakeOwner(*target.Owner); err != nil {
		return nil, nil
	}
	if !sameWakeOwner(target.Owner, inspection.Lock.Owner) {
		return nil, fmt.Errorf("persisted wake target and lock name different owners")
	}
	targetDigest, err := wakeTargetDigest(target)
	if err != nil {
		return nil, err
	}
	if targetDigest != inspection.Lock.TargetDigest {
		// A missing/older target is not part of the committed claim. Preserve it
		// as an orphan while recovery releases the authoritative lock.
		return nil, nil
	}
	if err := validateAuthoritativeWakeTarget(target, inspection.Root, inspection.Agent); err != nil {
		return nil, nil
	}
	if err := validateAuthoritativeWakeLockRecord(
		inspection.Lock,
		inspection.Root,
		inspection.Agent,
		target,
	); err != nil {
		return nil, err
	}
	return &target, nil
}

func acquireAuthoritativeWakeLockWithOptions(
	root string,
	me string,
	options wakeLockAcquireOptions,
) (func(), error) {
	requested := *options.target
	if err := validateAuthoritativeWakeTarget(requested, root, me); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(fsq.AgentBase(root, me), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create agent directory: %w", err)
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, err
	}
	defer func() { _ = agentDir.Close() }()

	for {
		var created wakeLockInspection
		var stopCapability *authoritativeWakeStopCapability
		fallbackOwnerless := false
		retry := false
		err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
			inspection := inspectWakeLockForOwnerTransition(dirfd, agentDir, root, me)
			claimClass := classifyPersistedWakeClaim(inspection)
			switch claimClass {
			case wakeClaimGeneric:
				return fmt.Errorf("wake handle %s has an ownerless wake; stop or retire it before starting an owner-bound wake", me)
			case wakeClaimInvalid:
				reason := inspection.Reason
				if reason == "" {
					reason = "persisted wake claim is not authoritative"
				}
				return fmt.Errorf("wake state for %s is unverified; refusing owner-bound acquisition: %s", me, reason)
			}

			requestedObservation, observeErr := observeAuthoritativeWakeOwner(*requested.Owner)
			defer func() { _ = requestedObservation.Close() }()
			if observeErr != nil {
				if requestedObservation.CapabilityUnsupported && claimClass == wakeClaimAbsent {
					fallbackOwnerless = true
					return nil
				}
				return observeErr
			}
			if requestedObservation.State != wakeOwnerSame {
				return fmt.Errorf("requested wake owner is %s: %s", requestedObservation.State, requestedObservation.Reason)
			}

			if claimClass == wakeClaimAuthoritative {
				persisted, err := validateAuthoritativeWakeClaimPairAt(dirfd, agentDir, inspection)
				if err != nil {
					return fmt.Errorf("persisted owner claim is unverified: %w", err)
				}
				persistedState := requestedObservation.State
				persistedReason := requestedObservation.Reason
				if !sameWakeOwner(inspection.Lock.Owner, requested.Owner) {
					persistedObservation, err := observeAuthoritativeWakeOwner(*inspection.Lock.Owner)
					defer func() { _ = persistedObservation.Close() }()
					if err != nil {
						return err
					}
					persistedState = persistedObservation.State
					persistedReason = persistedObservation.Reason
				}
				ownersEqual := sameWakeOwner(inspection.Lock.Owner, requested.Owner)
				wakeCapability, err := prepareAuthoritativeWakeStop(dirfd, agentDir, inspection)
				if err != nil {
					return fmt.Errorf("inspect authoritative wake through stable capability: %w", err)
				}
				keepWakeCapability := false
				defer func() {
					if !keepWakeCapability {
						_ = wakeCapability.Close()
					}
				}()
				classified := wakeCapability.Inspection
				wakeUsable := classified.Status == wakeLockValid &&
					classified.IdentityConfirmed &&
					wakeLockHasUsableNotificationPath(classified) &&
					ownersEqual &&
					sameWakeInjectorIdentity(persisted, requested)
				evidence := wakeOwnerTransitionEvidence{
					Request:            wakeOwnerRequestAcquire,
					Claim:              wakeClaimAuthoritative,
					RequestedState:     requestedObservation.State,
					PersistedState:     persistedState,
					OwnersEqual:        ownersEqual,
					WakeUsable:         wakeUsable,
					WakeExactStoppable: classified.Status == wakeLockValid && classified.IdentityConfirmed,
					WakeAbsent:         wakeCapability.Absent,
				}
				switch decideOwnerLifecycleTransition(evidence) {
				case wakeOwnerActionReuse:
					return wakeLockAlreadyRunningError(me, classified)
				case wakeOwnerActionRelease:
					if err := removeAuthoritativeWakeClaimAt(dirfd, agentDir, inspection, &persisted); err != nil {
						return err
					}
					retry = true
					return nil
				case wakeOwnerActionStopAndRelease:
					if wakeCapability.Absent {
						if err := removeAuthoritativeWakeClaimAt(dirfd, agentDir, inspection, &persisted); err != nil {
							return err
						}
						retry = true
						return nil
					}
					stopCapability = &wakeCapability
					keepWakeCapability = true
					return nil
				default:
					switch persistedState {
					case wakeOwnerSame:
						if !ownersEqual {
							return fmt.Errorf("wake handle %s is owned by live process pid %d, OS session %d", me, inspection.Lock.Owner.PID, inspection.Lock.Owner.SessionID)
						}
						return fmt.Errorf("wake handle %s has a live owner but an unusable wake; run 'amq wake recover-owner --me %s'", me, me)
					case wakeOwnerUnknown:
						return fmt.Errorf("wake owner for %s is unknown (%s); preserving owner claim", me, persistedReason)
					default:
						return fmt.Errorf("wake owner for %s cannot be safely reclaimed; run 'amq wake recover-owner --me %s'", me, me)
					}
				}
			}

			if orphan, exists, readErr := readWakeTargetAt(dirfd, agentDir, root, me); readErr != nil {
				return fmt.Errorf("uncommitted wake target is unverified: %w", readErr)
			} else if exists {
				if err := validateWakeTarget(orphan, root, me); err != nil {
					return fmt.Errorf("uncommitted wake target is invalid: %w", err)
				}
			}

			lock, err := newWakeLock(root, me, options)
			if err != nil {
				return err
			}
			if err := publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, me, requested, lock); err != nil {
				if errors.Is(err, errWakeOwnerLockExists) {
					retry = true
					return nil
				}
				var publicationErr *wakeOwnerPublicationError
				if errors.As(err, &publicationErr) {
					current := inspectWakeLockAt(dirfd, agentDir, root, me)
					if current.Exists {
						if current.Lock.Generation != lock.Generation {
							return fmt.Errorf("%w (a different wake lock became visible)", err)
						}
						if _, verifyErr := validateAuthoritativeWakeClaimPairAt(dirfd, agentDir, current); verifyErr != nil {
							return fmt.Errorf("%w (visible owner claim is unverified: %v)", err, verifyErr)
						}
						return fmt.Errorf("%w (the exact owner claim is visible and was preserved)", err)
					}
					if !publicationErr.Unsupported || publicationErr.Committed {
						return err
					}
					if unlinkErr := unix.Unlinkat(dirfd, wakeTargetFileName, 0); unlinkErr != nil && unlinkErr != unix.ENOENT {
						return fmt.Errorf("%w (remove uncommitted owner target: %v)", err, unlinkErr)
					}
					if syncErr := syncWakeOwnerDirFD(dirfd); syncErr != nil {
						return fmt.Errorf("%w (sync ownerless fallback cleanup: %v)", err, syncErr)
					}
					fallbackOwnerless = true
					return nil
				}
				return err
			}
			created = inspectWakeLockAt(dirfd, agentDir, root, me)
			if !created.Exists || created.Lock.Generation != lock.Generation {
				return fmt.Errorf("failed to verify created authoritative wake lock generation")
			}
			if _, err := validateAuthoritativeWakeClaimPairAt(dirfd, agentDir, created); err != nil {
				return fmt.Errorf("verify created authoritative wake claim: %w", err)
			}
			return nil
		})
		if err != nil {
			if stopCapability != nil {
				_ = stopCapability.Close()
			}
			return nil, err
		}
		if stopCapability != nil {
			stopErr := stopCapability.Stop(wakeOwnerReleaseAuthorization{})
			closeErr := stopCapability.Close()
			if stopErr != nil {
				return nil, stopErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			// No pre-wait evidence survives. Darwin may have removed the claim;
			// Linux leaves it for the next freshly guarded dead-owner pass.
			continue
		}
		if retry {
			continue
		}
		if fallbackOwnerless {
			ownerless := requested
			ownerless.Owner = nil
			_ = writeStderr("warning: owner-bound wake capabilities are unavailable; starting one ownerless inject-via wake\n")
			// Reflect the degradation in the caller's target too. The wake loop
			// must not keep applying owner-health semantics to a claim that was
			// deliberately published through the ownerless protocol.
			*options.target = ownerless
			options.wakeMode = wakeTargetInjectVia
			return acquireWakeLockWithOptions(root, me, options)
		}
		if !created.Exists {
			return nil, fmt.Errorf("authoritative wake acquisition produced no claim")
		}

		// Ordinary wake-loop termination is not an owner release. Recovery or
		// an exact in-process rollback owns authoritative claim removal.
		return func() {}, nil
	}
}
