//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

// wakeCheckSelfUpgradeObservation keeps the diagnostic files in the same
// stable snapshot as the wake lock. The diagnostic is advisory; restart
// refusal memory remains control-plane authority from .wake.restart.
type wakeCheckSelfUpgradeObservation struct {
	Sidecar       wakeCheckMetadataFingerprint
	Restart       wakeCheckMetadataFingerprint
	Diagnostic    wakeSelfUpgradeDiagnostic
	Present       bool
	RefusedMemory bool
}

func observeWakeCheckSelfUpgradeAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) wakeCheckSelfUpgradeObservation {
	var observation wakeCheckSelfUpgradeObservation
	if agentDir == nil {
		return observation
	}

	raw, info, exists, err := readWakeRepairMetadataAt(
		dirfd,
		wakeSelfUpgradeFileName,
		"wake self-upgrade diagnostic",
		filepath.Join(agentDir.path, wakeSelfUpgradeFileName),
		maxWakeMetadataFileBytes,
	)
	if err == nil {
		fingerprint, fingerprintErr := newWakeCheckMetadataFingerprint(exists, raw, info)
		if fingerprintErr == nil {
			observation.Sidecar = fingerprint
		}
		if exists && fingerprintErr == nil {
			var diagnostic wakeSelfUpgradeDiagnostic
			if json.Unmarshal(raw, &diagnostic) == nil &&
				validWakeCheckSelfUpgradeDiagnostic(diagnostic, inspection) {
				observation.Diagnostic = diagnostic
				observation.Present = true
			}
		}
	}

	restart, restartExists, restartErr := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
	if restartExists && restart.Object.FileInfo != nil {
		fingerprint, fingerprintErr := newWakeCheckMetadataFingerprint(
			true,
			restart.Object.Raw,
			restart.Object.FileInfo,
		)
		if fingerprintErr == nil {
			observation.Restart = fingerprint
		}
	}
	if restartErr == nil && restartExists &&
		restart.Record.Source == wakeRestartSourceSelf &&
		restart.Record.Status == wakeRestartRefused &&
		restart.Record.Generation == inspection.Lock.Generation {
		observation.RefusedMemory = true
	}
	return observation
}

func validWakeCheckSelfUpgradeDiagnostic(
	diagnostic wakeSelfUpgradeDiagnostic,
	inspection wakeLockInspection,
) bool {
	return diagnostic.Schema == wakeSelfUpgradeSchemaV1 &&
		diagnostic.Root == inspection.Root &&
		diagnostic.Agent == inspection.Agent &&
		diagnostic.Generation == inspection.Lock.Generation
}

func sameWakeCheckSelfUpgradeObservation(
	first, second wakeCheckSelfUpgradeObservation,
) bool {
	return first.Sidecar == second.Sidecar &&
		first.Restart == second.Restart &&
		first.Present == second.Present &&
		first.RefusedMemory == second.RefusedMemory
}

func wakeCheckSelfUpgradeDecisionFromObservation(
	observation wakeCheckSelfUpgradeObservation,
) wakeCheckSelfUpgradeDecision {
	decision := wakeCheckSelfUpgradeDecision{RefusedMemory: observation.RefusedMemory}
	if !observation.Present {
		return decision
	}
	diagnostic := observation.Diagnostic
	decision.Enabled = diagnostic.Enabled
	decision.Eligible = diagnostic.Eligible
	if diagnostic.Locator != "" {
		locator := diagnostic.Locator
		decision.Locator = &locator
	}
	if diagnostic.LastCandidate != nil {
		decision.LastCandidate = &wakeCheckSelfUpgradeCandidateDecision{
			Identity: wakeCheckSelfUpgradeIdentityString(diagnostic.LastCandidate.Identity),
			Version:  diagnostic.LastCandidate.Version,
		}
	}
	decision.LastDecision = &wakeCheckSelfUpgradeLastDecision{
		Action: diagnostic.LastDecision.Action,
		Reason: diagnostic.LastDecision.Reason,
		At:     diagnostic.LastDecision.At.UTC().Format(time.RFC3339Nano),
	}
	return decision
}

func wakeCheckSelfUpgradeIdentityString(identity wakeFileIdentity) string {
	return fmt.Sprintf(
		"dev=%d,ino=%d,ctime=%d.%09d",
		identity.Device,
		identity.Inode,
		identity.CTimeSec,
		identity.CTimeNsec,
	)
}
