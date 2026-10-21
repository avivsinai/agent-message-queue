//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWakeSelfUpgradeQuiescenceDefersThenRetries(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")

	bindCalls := 0
	previousBind := wakeRestartBind
	wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
		bindCalls++
		return nil, errors.New("test bind refusal")
	}
	t.Cleanup(func() { wakeRestartBind = previousBind })

	cfg := wakeConfig{
		me:                   fixture.agent,
		root:                 fixture.root,
		injectMode:           wakeInjectModeNone,
		wakeOwner:            &fixture.owner,
		terminalGeneration:   fixture.lock.Lock.Generation,
		terminalImageVersion: fixture.lock.Lock.ImageVersion,
		retainedAgent:        fixture.agentDir,
		retainedInbox:        fixture.inboxDir,
		selfUpgrade:          state,
		settings:             defaultWakeSettings(),
		inputDelivery: wakeInputDeliveryState{
			phase:         wakeInputPrimarySubmitPending,
			acceptedBytes: 1,
		},
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeLock(fixture.root, fixture.agent)
		},
		restartSignals: make(chan os.Signal, 1),
	}
	watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 0 {
		t.Fatalf("self-upgrade bound during delivery debt: bind calls=%d", bindCalls)
	}
	if _, err := os.Lstat(fixture.agentDir.path + "/" + wakeRestartFileName); !os.IsNotExist(err) {
		t.Fatalf("quiescence deferral published a restart record: %v", err)
	}
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionDeferred)

	cfg.inputDelivery.reset()
	if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
		t.Fatal(err)
	}
	if bindCalls != 1 {
		t.Fatalf("quiescent self-upgrade bind calls=%d, want 1", bindCalls)
	}
	assertWakeSelfUpgradeRecordStatus(t, fixture, wakeRestartRefused)
	assertWakeSelfUpgradeDiagnosticAction(t, fixture, wakeSelfUpgradeActionRefused)
}

// Issue 1022: the live self_upgrade setting applies on the next maintenance
// tick without a restart.
func TestWakeSelfUpgradeSettingAppliesLive(t *testing.T) {
	const (
		settingFalse = `{"schema":1,"settings":{"self_upgrade":false}}`
		settingTrue  = `{"schema":1,"settings":{"self_upgrade":true}}`
	)
	tests := []struct {
		name string
		// state builds the startup self-upgrade state.
		state func(t *testing.T) wakeSelfUpgradeState
		// settings is the file at each tick.
		settings    []string
		wantEnabled bool
		wantAction  string
		wantReason  string
	}{
		{
			name: "false stops a newer candidate",
			state: func(t *testing.T) wakeSelfUpgradeState {
				return selfUpgradeStateForCandidate(t, writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate"))
			},
			settings:   []string{settingFalse},
			wantAction: wakeSelfUpgradeActionDisabled,
			wantReason: "self_upgrade=false",
		},
		{
			// Regression: #1022, with the locator unchanged, later ticks return
			// early, so the veto record outlived the veto.
			name:        "true after false records the wake enabled again",
			state:       selfUpgradeStateForUnchangedLocator,
			settings:    []string{settingFalse, settingTrue},
			wantEnabled: true,
			wantAction:  wakeSelfUpgradeActionUnchanged,
		},
		{
			name: "true does not enable a wake started with --no-self-upgrade",
			state: func(*testing.T) wakeSelfUpgradeState {
				return captureWakeSelfUpgradeStartupState("amq", false, wakeImageEvidenceV1{})
			},
			settings:   []string{settingTrue},
			wantAction: wakeSelfUpgradeActionDisabled,
			wantReason: "restart to enable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newWakeRestartFixture(t)
			removeWakeRestartRecordForTest(t, fixture)
			state := tt.state(t)
			stubWakeSelfUpgradeVersion(t, "0.57.0")

			bindCalls := 0
			previousBind := wakeRestartBind
			wakeRestartBind = func(wakeRestartRecord) (*wakeRestartBoundImage, error) {
				bindCalls++
				return nil, errors.New("test bind refusal")
			}
			t.Cleanup(func() { wakeRestartBind = previousBind })

			var settings string
			cfg := wakeConfig{
				me:                   fixture.agent,
				root:                 fixture.root,
				injectMode:           wakeInjectModeNone,
				wakeOwner:            &fixture.owner,
				terminalGeneration:   fixture.lock.Lock.Generation,
				terminalImageVersion: fixture.lock.Lock.ImageVersion,
				retainedAgent:        fixture.agentDir,
				retainedInbox:        fixture.inboxDir,
				selfUpgrade:          state,
				settings:             defaultWakeSettings(),
				diagnosticIsTTY:      func() bool { return false },
				settingsSource: func() ([]byte, bool, error) {
					return []byte(settings), true, nil
				},
				inspectTerminalGeneration: func() wakeLockInspection {
					return inspectWakeLock(fixture.root, fixture.agent)
				},
				restartSignals: make(chan os.Signal, 1),
			}
			watcher := fixedWakeAdmissionWatcher{errors: make(chan error)}
			for _, settings = range tt.settings {
				cfg.reloadSettings()
				if err := maintainWakeSelfUpgradeAtLoopBoundary(&cfg, fixture.agentDir, watcher, false, false); err != nil {
					t.Fatal(err)
				}
			}
			if bindCalls != 0 {
				t.Fatalf("self-upgrade bound: bind calls=%d", bindCalls)
			}
			if _, err := os.Lstat(fixture.agentDir.path + "/" + wakeRestartFileName); !os.IsNotExist(err) {
				t.Fatalf("self-upgrade published a restart record: %v", err)
			}
			if err := fixture.agentDir.withFD(func(dirfd int) error {
				diagnostic, exists := readWakeSelfUpgradeDiagnosticAt(dirfd, fixture.agentDir, fixture.lock)
				if !exists || diagnostic.Enabled != tt.wantEnabled ||
					diagnostic.LastDecision.Action != tt.wantAction ||
					!strings.Contains(diagnostic.LastDecision.Reason, tt.wantReason) {
					return fmt.Errorf("diagnostic = %+v, want enabled=%t action=%s reason containing %q",
						diagnostic, tt.wantEnabled, tt.wantAction, tt.wantReason)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// selfUpgradeStateForUnchangedLocator is an eligible state whose stable
// locator still names the image it last probed: no candidate.
func selfUpgradeStateForUnchangedLocator(t *testing.T) wakeSelfUpgradeState {
	t.Helper()
	locator := filepath.Join(t.TempDir(), "amq")
	initial := writeWakeSelfUpgradeCandidate(t, filepath.Dir(locator), "initial")
	if err := os.Symlink(initial, locator); err != nil {
		t.Fatal(err)
	}
	probe, err := probeWakeSelfUpgradeLocator(locator)
	if err != nil {
		t.Fatal(err)
	}
	return wakeSelfUpgradeState{Enabled: true, Eligible: true, Locator: locator, lastProbe: probe}
}

func assertWakeSelfUpgradeRecordStatus(t *testing.T, fixture wakeRestartFixture, status string) {
	t.Helper()
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		record, exists, err := readWakeRestartRecordAt(dirfd, fixture.agentDir)
		if err != nil {
			return err
		}
		if !exists || record.Source != wakeRestartSourceSelf || record.Status != status {
			return errors.New("unexpected wake self-upgrade record")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertWakeSelfUpgradeDiagnosticAction(t *testing.T, fixture wakeRestartFixture, action string) {
	t.Helper()
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		diagnostic, exists := readWakeSelfUpgradeDiagnosticAt(dirfd, fixture.agentDir, fixture.lock)
		if !exists || diagnostic.LastDecision.Action != action {
			return errors.New("unexpected wake self-upgrade diagnostic")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
