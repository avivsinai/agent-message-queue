//go:build darwin || linux

package cli

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWakeCheckSelfUpgradeSnapshotMapsDiagnosticAndRefusalAuthority(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	decisionAt := time.Date(2026, time.August, 7, 12, 34, 56, 123456789, time.FixedZone("UTC+3", 3*60*60))
	diagnostic := wakeSelfUpgradeDiagnostic{
		Schema:     wakeSelfUpgradeSchemaV1,
		Root:       fixture.root,
		Agent:      fixture.agent,
		Generation: fixture.lock.Lock.Generation,
		Enabled:    true,
		Eligible:   true,
		Locator:    "/opt/homebrew/bin/amq",
		LastCandidate: &wakeSelfUpgradeCandidate{
			Identity: wakeFileIdentity{Device: 7, Inode: 11, CTimeSec: 13, CTimeNsec: 17},
			Version:  "0.59.0",
		},
		LastDecision: wakeSelfUpgradeDiagnosticDecision{
			Action: wakeSelfUpgradeActionRefused,
			Reason: "candidate was declined",
			At:     decisionAt,
		},
	}
	writeWakeCheckSelfUpgradeDiagnostic(t, fixture, diagnostic)

	record := fixture.record
	record.Source = wakeRestartSourceSelf
	record.Status = wakeRestartRefused
	record.Reason = "candidate was declined"
	writeWakeCheckSelfUpgradeRestartRecord(t, fixture, record)

	snapshot := inspectWakeCheckSnapshot(fixture.root, fixture.agent)
	got := snapshot.Decision.SelfUpgrade
	if !got.Enabled || !got.Eligible || got.Locator == nil || *got.Locator != diagnostic.Locator {
		t.Fatalf("self-upgrade state = %#v", got)
	}
	if got.LastCandidate == nil || got.LastCandidate.Identity != "dev=7,ino=11,ctime=13.000000017" ||
		got.LastCandidate.Version != "0.59.0" {
		t.Fatalf("candidate = %#v", got.LastCandidate)
	}
	if got.LastDecision == nil || got.LastDecision.Action != wakeSelfUpgradeActionRefused ||
		got.LastDecision.Reason != "candidate was declined" ||
		got.LastDecision.At != "2026-08-07T09:34:56.123456789Z" {
		t.Fatalf("last decision = %#v", got.LastDecision)
	}
	if !got.RefusedMemory {
		t.Fatal("refused memory = false, want true from valid self restart record")
	}

	rendered := renderWakeCheckV2(snapshot.Decision).SelfUpgrade
	if !rendered.RefusedMemory || rendered.LastCandidate == nil || rendered.LastDecision == nil {
		t.Fatalf("v2 self-upgrade = %#v", rendered)
	}
}

func TestWakeCheckSelfUpgradeObservationTreatsDiagnosticFaultsAsNoData(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	diagnostic := wakeSelfUpgradeDiagnostic{
		Schema:     wakeSelfUpgradeSchemaV1,
		Root:       fixture.root,
		Agent:      fixture.agent,
		Generation: fixture.lock.Lock.Generation,
		Enabled:    true,
		LastDecision: wakeSelfUpgradeDiagnosticDecision{
			Action: wakeSelfUpgradeActionUnchanged,
			At:     time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC),
		},
	}
	writeWakeCheckSelfUpgradeDiagnostic(t, fixture, diagnostic)

	first, err := observeWakeCheck(fixture.root, fixture.agent)
	if err != nil || !first.SelfUpgrade.Present {
		t.Fatalf("first self-upgrade observation = %#v, err = %v", first.SelfUpgrade, err)
	}
	diagnostic.LastDecision.Reason = "changed without changing the lock"
	writeWakeCheckSelfUpgradeDiagnostic(t, fixture, diagnostic)
	second, err := observeWakeCheck(fixture.root, fixture.agent)
	if err != nil || sameWakeCheckObservation(first, second) {
		t.Fatalf("changed sidecar observation = %#v, err = %v", second.SelfUpgrade, err)
	}

	diagnostic.Generation = "stale-generation"
	writeWakeCheckSelfUpgradeDiagnostic(t, fixture, diagnostic)
	stale, err := observeWakeCheck(fixture.root, fixture.agent)
	if err != nil || stale.SelfUpgrade.Present {
		t.Fatalf("stale diagnostic observation = %#v, err = %v", stale.SelfUpgrade, err)
	}

	writeWakeCheckSelfUpgradeRaw(t, fixture, []byte("{not-json}\n"))
	corrupt, err := observeWakeCheck(fixture.root, fixture.agent)
	if err != nil || corrupt.SelfUpgrade.Present || !corrupt.SelfUpgrade.Sidecar.Exists {
		t.Fatalf("corrupt diagnostic observation = %#v, err = %v", corrupt.SelfUpgrade, err)
	}

	writeWakeCheckRestartRaw(t, fixture, []byte("{not-json}\n"))
	invalidRestart, err := observeWakeCheck(fixture.root, fixture.agent)
	if err != nil || invalidRestart.SelfUpgrade.RefusedMemory || !invalidRestart.SelfUpgrade.Restart.Exists {
		t.Fatalf("invalid restart observation = %#v, err = %v", invalidRestart.SelfUpgrade, err)
	}
}

func writeWakeCheckSelfUpgradeDiagnostic(
	t *testing.T,
	fixture wakeRestartFixture,
	diagnostic wakeSelfUpgradeDiagnostic,
) {
	t.Helper()
	raw, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	writeWakeCheckSelfUpgradeRaw(t, fixture, append(raw, '\n'))
}

func writeWakeCheckSelfUpgradeRaw(t *testing.T, fixture wakeRestartFixture, raw []byte) {
	t.Helper()
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRepairMetadataAt(
			dirfd,
			fixture.agentDir,
			wakeSelfUpgradeFileName,
			"wake self-upgrade diagnostic",
			raw,
			maxWakeMetadataFileBytes,
		)
	}); err != nil {
		t.Fatal(err)
	}
}

func writeWakeCheckSelfUpgradeRestartRecord(
	t *testing.T,
	fixture wakeRestartFixture,
	record wakeRestartRecord,
) {
	t.Helper()
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
	}); err != nil {
		t.Fatal(err)
	}
}

func writeWakeCheckRestartRaw(t *testing.T, fixture wakeRestartFixture, raw []byte) {
	t.Helper()
	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return writeWakeRepairMetadataAt(
			dirfd,
			fixture.agentDir,
			wakeRestartFileName,
			"wake restart request",
			raw,
			maxWakeMetadataFileBytes,
		)
	}); err != nil {
		t.Fatal(err)
	}
}
