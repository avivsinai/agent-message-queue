//go:build !darwin && !linux

package cli

import (
	"errors"
	"flag"
	"os"
	"runtime"
)

func decorateOpsWakeLockWithWakeCheck(
	root, agent string,
	lock *opsWakeLock,
	_ wakeLockInspection,
	_ bool,
	includeV2 bool,
) {
	if !includeV2 {
		return
	}
	decision := unsupportedWakeCheckDecision(root, agent)
	lock.WakeCheckDecision = &decision
}

func runWakeCheckUnsupported(args []string) error {
	fs := flag.NewFlagSet("wake check", flag.ContinueOnError)
	common := addCommonFlags(fs)
	jsonSchema := addJSONSchemaFlag(fs)
	usage := usageWithFlags(
		fs,
		"amq wake check --me <agent> [options]",
		"Inspect wake start and restart capability without mutation.",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := validateJSONSchemaFlag(fs, common.JSON, *jsonSchema); err != nil {
		return err
	}
	if !common.JSON || *jsonSchema == wakeCheckSchemaV1 {
		return errors.New("amq wake is not supported on this platform (requires macOS or Linux)")
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
	return writeJSON(os.Stdout, renderWakeCheckV2(unsupportedWakeCheckDecision(root, me)))
}

func unsupportedWakeCheckDecision(root, agent string) wakeCheckDecision {
	reason := wakeReasonPlatformUnsupported
	detail := "amq wake is not supported on this platform"
	return wakeCheckDecision{
		Agent: agent,
		Root:  canonicalWakeRoot(root),
		Platform: wakeCheckPlatformDecision{
			OS:            runtime.GOOS,
			WakeSupported: false,
			ReasonCode:    &reason,
		},
		Start: wakeCheckStartDecision{
			Available:  false,
			Mode:       wakeInjectModeNone,
			ReasonCode: &reason,
			Detail:     &detail,
		},
		Wake: wakeCheckWakeDecision{
			Status: string(wakeLockMissing),
		},
		Image: wakeCheckImageDecision{
			Current: wakeCheckImageEvidenceDecision{
				Path:    wakeCheckOptionalString(wakeCheckActionProgram()),
				Version: wakeCheckOptionalString(cliVersion),
			},
			Status: wakeImageUnknown,
		},
		Repair: wakeCheckRepairDecision{
			ReasonCode:   &reason,
			Detail:       &detail,
			legacyReason: detail,
		},
		Reload: wakeCheckReloadDecision{
			Status:     wakeReloadUnavailable,
			ReasonCode: wakeReloadReasonPlatformUnsupported,
		},
		RestartCapability: wakeRestartUnavailable,
		Action: wakeCheckActionDecision{
			Kind:       wakeActionUnsupported,
			Actor:      wakeActionActorNone,
			ReasonCode: reason,
			Message:    detail,
		},
	}
}
