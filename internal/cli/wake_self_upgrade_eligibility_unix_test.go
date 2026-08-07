//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWakeSelfUpgradeStartupEligibilityRejectsUnsafeWakeShapes(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	locator, running := wakeSelfUpgradeStableLocatorForEligibilityTest(t)
	owner := fixture.owner

	previousVersion := wakeSelfUpgradeRunVersion
	previousLiveDifference := wakeSelfUpgradeLiveDifference
	previousBind := wakeRestartBind
	versionCalls := 0
	bindCalls := 0
	wakeSelfUpgradeRunVersion = func(string) (string, error) {
		versionCalls++
		return "999.0.0", nil
	}
	wakeSelfUpgradeLiveDifference = func(wakeLockInspection, wakeSelfUpgradeProbe) (bool, error) {
		return false, nil
	}
	wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalls++
		return nil, os.ErrPermission
	}
	t.Cleanup(func() {
		wakeSelfUpgradeRunVersion = previousVersion
		wakeSelfUpgradeLiveDifference = previousLiveDifference
		wakeRestartBind = previousBind
	})

	tests := []struct {
		name          string
		owner         *wakeOwner
		repairLineage bool
		injectCmd     string
		interruptKey  string
		wakeMode      string
		flagDisabled  bool
		envDisabled   bool
	}{
		{
			name:     "default cooperative raw wake",
			owner:    &owner,
			wakeMode: wakeInjectModeRaw,
		},
		{name: "ownerless inject-via", wakeMode: wakeTargetInjectVia},
		{name: "ownerless keepalive", wakeMode: wakeInjectModeNone},
		{name: "standalone raw", wakeMode: wakeInjectModeRaw},
		{name: "standalone paste", wakeMode: wakeInjectModePaste},
		{
			name:          "repair lineage",
			owner:         &owner,
			repairLineage: true,
			wakeMode:      wakeTargetInjectVia,
		},
		{
			name:      "arbitrary inject command",
			owner:     &owner,
			injectCmd: "tmux send-keys",
			wakeMode:  wakeInjectModeRaw,
		},
		{
			name:         "destructive interrupt",
			owner:        &owner,
			interruptKey: "\x03",
			wakeMode:     wakeInjectModeRaw,
		},
		{
			name:         "explicit flag disable",
			owner:        &owner,
			wakeMode:     wakeInjectModeRaw,
			flagDisabled: true,
		},
		{
			name:        "environment disable",
			owner:       &owner,
			wakeMode:    wakeInjectModeRaw,
			envDisabled: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.envDisabled {
				t.Setenv(envWakeNoSelfUpgrade, "1")
			}
			startupEligible := wakeResumeStartupEligible(
				test.owner,
				test.repairLineage,
				test.injectCmd,
				test.interruptKey,
				test.wakeMode,
			)
			operatorEnabled := !test.flagDisabled && !wakeSelfUpgradeDisabledByEnv()
			state := constrainWakeSelfUpgradeEligibility(
				captureWakeSelfUpgradeStartupState(locator, operatorEnabled, running),
				startupEligible,
			)
			wantEnabled := operatorEnabled
			wantEligible := operatorEnabled && startupEligible
			if state.Enabled != wantEnabled || state.Eligible != wantEligible {
				t.Fatalf("startup state = %#v, want enabled=%v eligible=%v", state, wantEnabled, wantEligible)
			}

			versionBefore, bindBefore := versionCalls, bindCalls
			decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
			if err != nil {
				t.Fatal(err)
			}
			wantAction := wakeSelfUpgradeActionUnchanged
			if !operatorEnabled {
				wantAction = wakeSelfUpgradeActionDisabled
			} else if !startupEligible {
				wantAction = wakeSelfUpgradeActionIneligible
			}
			if decision.Action != wantAction {
				t.Fatalf("startup decision = %#v, want action %q", decision, wantAction)
			}
			if versionCalls != versionBefore || bindCalls != bindBefore {
				t.Fatalf("unsafe shape performed version/bind work: versions=%d binds=%d", versionCalls-versionBefore, bindCalls-bindBefore)
			}
			if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
				t.Fatalf("startup shape published a restart record: %v (decision=%#v)", err, decision)
			}
		})
	}
}

func wakeSelfUpgradeStableLocatorForEligibilityTest(t *testing.T) (string, wakeImageEvidenceV1) {
	t.Helper()
	dir := t.TempDir()
	target := writeWakeSelfUpgradeCandidate(t, dir, "amq")
	running, err := captureWakeImageEvidence(target, "0.58.0")
	if err != nil {
		t.Fatal(err)
	}
	locator := filepath.Join(dir, "stable-amq")
	if err := os.Symlink(target, locator); err != nil {
		t.Fatal(err)
	}
	return locator, running
}
