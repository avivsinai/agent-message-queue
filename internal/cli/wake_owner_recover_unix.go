//go:build darwin || linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

type wakeOwnerRecoverResult struct {
	Status       string `json:"status"`
	Agent        string `json:"agent"`
	Root         string `json:"root"`
	Lock         string `json:"lock"`
	Target       string `json:"target,omitempty"`
	PID          int    `json:"pid,omitempty"`
	OwnerPID     int    `json:"owner_pid,omitempty"`
	OwnerSession int    `json:"owner_session,omitempty"`
	Reason       string `json:"reason,omitempty"`
	NextAction   string `json:"next_action,omitempty"`
}

func runWakeRecoverOwner(args []string) error {
	fs := flag.NewFlagSet("wake recover-owner", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(
		fs,
		"amq wake recover-owner --me <agent> [options]",
		"Recover one exact owner-bound wake claim.",
		"",
		"Live release requires the exact AMQ_WAKE_OWNER token and the caller's",
		"current OS session to match the persisted owner. There is no force mode.",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	result, recoverErr := recoverOwnerWake(root, me)
	if common.JSON {
		if err := writeJSON(os.Stdout, result); err != nil {
			return err
		}
		return recoverErr
	}
	line := fmt.Sprintf("wake recover-owner: %s agent=%s root=%s", result.Status, result.Agent, result.Root)
	if result.PID != 0 {
		line += fmt.Sprintf(" pid=%d", result.PID)
	}
	if result.OwnerPID != 0 {
		line += fmt.Sprintf(" owner_pid=%d owner_session=%d", result.OwnerPID, result.OwnerSession)
	}
	if result.Reason != "" {
		line += " reason=" + result.Reason
	}
	if result.NextAction != "" {
		line += " next_action=" + result.NextAction
	}
	if err := writeStdoutLine(line); err != nil {
		return err
	}
	return recoverErr
}

func recoverOwnerWake(root, me string) (wakeOwnerRecoverResult, error) {
	result := wakeOwnerRecoverResult{
		Status: "unknown",
		Agent:  me,
		Root:   canonicalWakeRoot(root),
		Lock:   filepath.Join(fsq.AgentBase(root, me), ".wake.lock"),
		Target: wakeTargetPath(root, me),
	}
	if err := os.MkdirAll(fsq.AgentBase(root, me), 0o700); err != nil {
		result.Status = "error"
		result.Reason = err.Error()
		return result, err
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		result.Status = "error"
		result.Reason = err.Error()
		return result, err
	}
	defer func() { _ = agentDir.Close() }()

	refuse := func(reason, next string) error {
		result.Status = "refused"
		result.Reason = reason
		result.NextAction = next
		return errors.New(reason)
	}

	for {
		var stopCapability *authoritativeWakeStopCapability
		var stopAuthorization wakeOwnerReleaseAuthorization
		err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
			inspection := inspectWakeLockForOwnerTransition(dirfd, agentDir, root, me)
			result.PID = inspection.PID
			switch classifyPersistedWakeClaim(inspection) {
			case wakeClaimAbsent:
				target, exists, err := readWakeTargetAt(dirfd, agentDir, root, me)
				if err != nil {
					return refuse("orphan wake target is unverified: "+err.Error(), "inspect the target and retry")
				}
				if exists {
					digest, err := wakeTargetDigest(target)
					if err != nil {
						return refuse("orphan wake target digest is unavailable: "+err.Error(), "inspect the target and retry")
					}
					current, currentExists, err := readWakeTargetAt(dirfd, agentDir, root, me)
					if err != nil {
						return refuse("orphan wake target changed while recovering: "+err.Error(), "retry")
					}
					currentDigest, digestErr := wakeTargetDigest(current)
					if digestErr != nil {
						return refuse("orphan wake target digest changed while recovering: "+digestErr.Error(), "retry")
					}
					if !currentExists || currentDigest != digest {
						return refuse("orphan wake target changed while recovering", "retry")
					}
					if err := unix.Unlinkat(dirfd, wakeTargetFileName, 0); err != nil && err != unix.ENOENT {
						return fmt.Errorf("remove orphan wake target: %w", err)
					}
					if err := syncWakeOwnerDirFD(dirfd); err != nil {
						return fmt.Errorf("sync orphan wake target removal: %w", err)
					}
				}
				result.Status = "recovered"
				result.Reason = "owner claim is absent"
				return nil
			case wakeClaimGeneric:
				return refuse("wake lock is ownerless; recover-owner cannot mutate it", "use wake repair or wake retire")
			case wakeClaimInvalid:
				return refuse("wake owner claim is unverified or corrupt", "inspect the claim and retry without mutating it")
			}

			target, err := authoritativeWakeRecoveryTargetAt(dirfd, agentDir, inspection)
			if err != nil {
				return refuse("wake owner claim is unverified: "+err.Error(), "repair the metadata manually only after preserving evidence")
			}
			owner := *inspection.Lock.Owner
			result.OwnerPID = owner.PID
			result.OwnerSession = owner.SessionID
			observation, err := observeAuthoritativeWakeOwner(owner)
			defer func() { _ = observation.Close() }()
			if err != nil {
				return refuse("wake owner is unknown: "+err.Error(), "retry after owner identity inspection is available")
			}

			authenticated := false
			var token *wakeOwner
			if observation.State == wakeOwnerSame {
				token, err = wakeOwnerFromEnv()
				if err != nil {
					return refuse("wake owner token is invalid: "+err.Error(), "run recovery from the owning process session")
				}
				if token == nil {
					return refuse("wake owner token is missing", "run recovery from the owning process session")
				}
				if err := validateAuthoritativeWakeOwner(*token); err != nil {
					return refuse("wake owner token is not authoritative: "+err.Error(), "run recovery from the owning process session")
				}
				if !sameWakeOwner(token, &owner) {
					return refuse("wake owner token does not match the persisted owner", "run recovery from the owning process session")
				}
				callerSession, err := getWakeProcessSID(os.Getpid())
				if err != nil {
					return refuse("recovery caller OS session unavailable: "+err.Error(), "retry from a process with a readable OS session")
				}
				if callerSession != owner.SessionID {
					return refuse(
						fmt.Sprintf("recovery caller OS session %d does not match owner OS session %d", callerSession, owner.SessionID),
						fmt.Sprintf("exit or stop owner pid %d, then rerun amq wake recover-owner --me %s", owner.PID, me),
					)
				}
				authenticated = true
			}

			if observation.State == wakeOwnerUnknown {
				return refuse(
					"wake owner is unknown: "+observation.Reason,
					"retry after owner identity inspection is available",
				)
			}
			wakeCapability, err := prepareAuthoritativeWakeStop(dirfd, agentDir, inspection)
			if err != nil {
				return refuse("exact wake inspection is unavailable: "+err.Error(), "preserve the claim and retry")
			}
			keepWakeCapability := false
			defer func() {
				if !keepWakeCapability {
					_ = wakeCapability.Close()
				}
			}()
			classified := wakeCapability.Inspection
			action := decideOwnerLifecycleTransition(wakeOwnerTransitionEvidence{
				Request:            wakeOwnerRequestRecover,
				Claim:              wakeClaimAuthoritative,
				PersistedState:     observation.State,
				Authenticated:      authenticated,
				WakeExactStoppable: classified.Status == wakeLockValid && classified.IdentityConfirmed,
				WakeAbsent:         wakeCapability.Absent,
			})
			switch action {
			case wakeOwnerActionRelease:
				if err := removeAuthoritativeWakeClaimAt(dirfd, agentDir, inspection, target); err != nil {
					return err
				}
				result.Status = "recovered"
				result.Reason = "exact owner claim released"
				return nil
			case wakeOwnerActionStopAndRelease:
				if wakeCapability.Absent {
					if err := removeAuthoritativeWakeClaimAt(dirfd, agentDir, inspection, target); err != nil {
						return err
					}
					result.Status = "recovered"
					result.Reason = "exact owner claim released"
					return nil
				}
				stopCapability = &wakeCapability
				keepWakeCapability = true
				stopAuthorization = wakeOwnerReleaseAuthorization{Token: token}
				return nil
			default:
				switch observation.State {
				case wakeOwnerUnknown:
					return refuse(
						"wake owner is unknown: "+observation.Reason,
						"retry after owner identity inspection is available",
					)
				case wakeOwnerSame:
					return refuse(
						fmt.Sprintf("handle %s is owned by live process pid %d, OS session %d", me, owner.PID, owner.SessionID),
						fmt.Sprintf("exit or stop that process, then rerun amq wake recover-owner --me %s", me),
					)
				default:
					return refuse("wake process identity is not exactly stoppable", "preserve the claim and retry")
				}
			}
		})
		if err != nil {
			if stopCapability != nil {
				_ = stopCapability.Close()
			}
			if result.Status == "unknown" {
				result.Status = "error"
				result.Reason = err.Error()
			}
			return result, err
		}
		if stopCapability == nil {
			return result, nil
		}
		stopErr := stopCapability.Stop(stopAuthorization)
		closeErr := stopCapability.Close()
		if stopErr != nil {
			result.Status = "error"
			result.Reason = stopErr.Error()
			return result, stopErr
		}
		if closeErr != nil {
			result.Status = "error"
			result.Reason = closeErr.Error()
			return result, closeErr
		}
		// Stop waits happen without the guard. Re-enter from a completely fresh
		// persisted and process observation; no authorization decision survives.
	}
}
