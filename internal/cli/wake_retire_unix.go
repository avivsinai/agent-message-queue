//go:build darwin || linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type wakeRetireResult struct {
	Status string `json:"status"`
	Agent  string `json:"agent"`
	Root   string `json:"root"`
	Lock   string `json:"lock"`
	Target string `json:"target,omitempty"`
	PID    int    `json:"pid,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func runWakeRetire(args []string) error {
	fs := flag.NewFlagSet("wake retire", flag.ContinueOnError)
	common := addCommonFlags(fs)
	injectViaFlag := fs.String("inject-via", "", "Expected external injection executable")
	var injectArgFlags multiStringFlag
	fs.Var(&injectArgFlags, "inject-arg", "Expected fixed injection argument (repeatable)")
	usage := usageWithFlags(fs, "amq wake retire --me <agent> --inject-via <path> [options]",
		"Stop an identity-confirmed live inject-via wake or remove its exactly-bound proven-stale lock.",
		"",
		"The expected executable and ordered arguments must exactly match the saved target.",
		"Retirement preserves the mailbox and saved target and never stops raw wakes.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if strings.TrimSpace(*injectViaFlag) == "" {
		return UsageError("--inject-via is required")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	requested, err := newWakeTarget(root, me, *injectViaFlag, []string(injectArgFlags))
	if err != nil {
		return UsageError("--inject-via: %v", err)
	}
	result, retireErr := retireWake(root, me, requested)
	if common.JSON {
		if err := writeJSON(os.Stdout, result); err != nil {
			return err
		}
		return retireErr
	}
	line := fmt.Sprintf("wake retire: %s agent=%s root=%s", result.Status, result.Agent, result.Root)
	if result.PID != 0 {
		line += fmt.Sprintf(" pid=%d", result.PID)
	}
	if result.Reason != "" {
		line += " reason=" + result.Reason
	}
	if err := writeStdoutLine(line); err != nil {
		return err
	}
	return retireErr
}

func retireWake(root, me string, requested wakeTarget) (wakeRetireResult, error) {
	result := wakeRetireResult{
		Status: "unknown",
		Agent:  me,
		Root:   canonicalWakeRoot(root),
		Lock:   filepath.Join(fsq.AgentBase(root, me), ".wake.lock"),
		Target: wakeTargetPath(root, me),
	}
	refuse := func(reason string) (wakeRetireResult, error) {
		result.Status = "refused"
		result.Reason = reason
		return result, errors.New(reason)
	}
	fail := func(err error) (wakeRetireResult, error) {
		result.Status = "error"
		result.Reason = err.Error()
		return result, err
	}

	inspection := inspectWakeLock(root, me)
	if !inspection.Exists {
		return refuse("no wake lock present; wake process absence cannot be proven")
	}
	result.PID = inspection.PID
	if wakeLockHasOwnerMarkers(inspection) {
		return refuse(fmt.Sprintf("owner-bound wake claims require 'amq wake recover-owner --me %s'", me))
	}

	switch inspection.Status {
	case wakeLockValid:
		if !inspection.IdentityConfirmed {
			return refuse("wake process identity is not confirmed")
		}
		if inspection.Lock.WakeMode != wakeTargetInjectVia {
			return refuse("live raw wake retirement is owned by its terminal or supervisor")
		}

		var confirmed wakeLockInspection
		if err := withWakeLifecycleGuard(root, me, func() error {
			confirmed = inspectWakeLock(root, me)
			if !sameWakeLockInspection(inspection, confirmed) || !confirmed.IdentityConfirmed {
				return errors.New("wake lock changed before retirement")
			}
			return requireExistingWakeTargetMatches(confirmed, requested)
		}); err != nil {
			return refuse(err.Error())
		}

		retired, err := terminateAndRemoveOrphanedWakeLock(confirmed)
		if err != nil {
			return fail(err)
		}
		if !retired {
			return refuse("wake lock or process identity changed before retirement")
		}
		result.Status = "retired"
		result.Reason = "live inject-via wake stopped; mailbox and saved target preserved"
		return result, nil

	case wakeLockStale:
		removeFailed := false
		err := withWakeLifecycleGuard(root, me, func() error {
			current := inspectWakeLock(root, me)
			if !sameWakeLockGeneration(inspection, current) || current.Status != wakeLockStale {
				return errors.New("wake lock changed before retirement")
			}
			if err := validateWakeLockStaleRemoval(current); err != nil {
				return err
			}
			if err := requireExistingWakeTargetMatches(current, requested); err != nil {
				return err
			}
			if err := removeWakeLockIfUnchangedGuarded(current); err != nil {
				removeFailed = true
				return err
			}
			return nil
		})
		if err != nil {
			if removeFailed {
				return fail(err)
			}
			return refuse(err.Error())
		}
		result.Status = "retired"
		result.Reason = "exactly-bound proven-stale wake lock removed; mailbox and saved target preserved"
		return result, nil

	case wakeLockCreating:
		return refuse("wake lock is being created; retry shortly")
	case wakeLockUnverified:
		return refuse("wake lock is unverified; refusing retirement: " + inspection.Reason)
	default:
		return refuse(fmt.Sprintf("wake lock status %q cannot be retired", inspection.Status))
	}
}

func requireExistingWakeTargetMatches(inspection wakeLockInspection, requested wakeTarget) error {
	persisted, exists, err := readWakeTarget(inspection.Root, inspection.Agent)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("no saved inject-via wake target; refusing retirement")
	}
	if persisted.Owner != nil {
		return fmt.Errorf("owner-bearing wake state requires 'amq wake recover-owner --me %s'", inspection.Agent)
	}
	if err := validateWakeTarget(persisted, inspection.Root, inspection.Agent); err != nil {
		return err
	}
	if err := validateWakeTargetMatchesLock(inspection.Lock, persisted); err != nil {
		return err
	}
	if !sameWakeInjectorIdentity(persisted, requested) {
		return errors.New("saved wake target uses a different injector path or fixed arguments")
	}
	return nil
}
