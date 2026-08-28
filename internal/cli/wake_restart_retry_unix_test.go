//go:build darwin || linux

package cli

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestWakeRestartRetriesReadinessWhenRecordChangesWhileOpening proves the
// requestWakeRestart poll loop treats wakeSnapshotReadChangedError as transient:
// the first readiness observation fails with the typed error (the owner rewrote
// the record by atomic rename between the reader's two opens), and the second
// observation sees the stable restarted readiness. Before the fix, the first
// error aborted the whole restart with status=refused.
func TestWakeRestartRetriesReadinessWhenRecordChangesWhileOpening(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	bound := boundWakeImageEvidenceForTest(fixture.candidate)
	prepareWakeRestartRecordForBoundResumeTest(t, &fixture, &bound)
	cleanup, err := acquireWakeLockAfterResumeInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		wakeLockAcquireOptions{
			wakeMode:            wakeInjectModeNone,
			requestedOwner:      &fixture.owner,
			resumeEligible:      true,
			resumeImageEvidence: &bound,
		},
		wakeResumeBootstrap{
			Schema:     wakeRestartSchemaV1,
			RequestID:  fixture.record.RequestID,
			Generation: fixture.record.Generation,
			BoundImage: &bound,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	current := inspectWakeLock(fixture.root, fixture.agent)

	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	oldSleep := wakeRestartSleep
	oldObserve := wakeRestartObserveReadiness
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
		wakeRestartSleep = oldSleep
		wakeRestartObserveReadiness = oldObserve
	})
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error { return nil }
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error { return nil }

	// On the first sleep, complete the owner-side restart: write the prepared
	// proof and consume the restart record so the next readiness observation is
	// stable and ready. The transient error is injected by observeCalls below.
	sleepCalls := 0
	wakeRestartSleep = func(time.Duration) {
		sleepCalls++
		if sleepCalls != 1 {
			return
		}
		if err := writeWakePreparedFileInDir(
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			current,
		); err != nil {
			t.Fatal(err)
		}
		if err := consumeWakeRestartAfterPrepared(
			fixture.agentDir,
			fixture.root,
			fixture.agent,
			current,
			wakeResumeBootstrap{
				Schema:     wakeRestartSchemaV1,
				RequestID:  fixture.record.RequestID,
				Generation: fixture.record.Generation,
			},
		); err != nil {
			t.Fatal(err)
		}
	}

	observeCalls := 0
	sawTransient := false
	wakeRestartObserveReadiness = func(
		agentDir *wakeAgentDir,
		root, me string,
		expected wakeLockInspection,
	) (wakeRestartReadiness, error) {
		observeCalls++
		if observeCalls == 1 {
			sawTransient = true
			// The exact error the producer returns when the record's inode/ctime
			// changes between the reader's two opens (see readWakeQuarantineSnapshotAt).
			return wakeRestartReadiness{}, newWakeSnapshotReadChangedError(
				errors.New("wake restart request changed while opening"),
			)
		}
		return oldObserve(agentDir, root, me, expected)
	}

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err != nil {
		t.Fatalf("transient readiness error should retry to success: result=%#v err=%v", result, err)
	}
	if result.Status != "restarted" || result.PreviousGeneration != fixture.record.Generation ||
		result.Generation != current.Lock.Generation {
		t.Fatalf("restarted result after transient retry = %#v", result)
	}
	if !sawTransient {
		t.Fatalf("transient readiness error was never injected; test does not exercise the retry path (observeCalls=%d)", observeCalls)
	}
	if observeCalls < 2 {
		t.Fatalf("readiness observer was not retried after the transient error (observeCalls=%d)", observeCalls)
	}
}

// TestWakeRestartStillRefusesWhenRecordStaysRefused is the negative guard: a
// record that stays status=refused must still terminate as refused. A naive
// "swallow all read errors" implementation would loop to the restart timeout
// instead; this test pins that only wakeSnapshotReadChangedError is retried.
func TestWakeRestartStillRefusesWhenRecordStaysRefused(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	bound := boundWakeImageEvidenceForTest(fixture.candidate)
	prepareWakeRestartRecordForBoundResumeTest(t, &fixture, &bound)
	cleanup, err := acquireWakeLockAfterResumeInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		wakeLockAcquireOptions{
			wakeMode:            wakeInjectModeNone,
			requestedOwner:      &fixture.owner,
			resumeEligible:      true,
			resumeImageEvidence: &bound,
		},
		wakeResumeBootstrap{
			Schema:     wakeRestartSchemaV1,
			RequestID:  fixture.record.RequestID,
			Generation: fixture.record.Generation,
			BoundImage: &bound,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	oldPreflight := wakeRestartPreflight
	oldNotify := wakeRestartNotify
	oldSleep := wakeRestartSleep
	oldObserve := wakeRestartObserveReadiness
	oldReadRecord := wakeRestartReadRecord
	t.Cleanup(func() {
		wakeRestartPreflight = oldPreflight
		wakeRestartNotify = oldNotify
		wakeRestartSleep = oldSleep
		wakeRestartObserveReadiness = oldObserve
		wakeRestartReadRecord = oldReadRecord
	})
	wakeRestartPreflight = func(wakeImageEvidenceV1, []string, wakeResumeBootstrap) error { return nil }
	wakeRestartNotify = func(*wakeAgentDir, wakeLockInspection, wakeRestartRecord) error { return nil }

	// Bypass the real sleep so the loop spins fast; the refused record must
	// terminate it well before any real wall-clock timeout.
	wakeRestartSleep = func(time.Duration) {}

	// Simulate a generation change so the poll loop reaches the refused-record
	// check. The readiness observer reports prepared=false with the refused
	// record present, then the explicit record read returns the refused record.
	refusedRecord := fixture.record
	refusedRecord.Status = wakeRestartRefused
	refusedRecord.Reason = "owner refused restart for test"
	wakeRestartObserveReadiness = func(
		*wakeAgentDir,
		string,
		string,
		wakeLockInspection,
	) (wakeRestartReadiness, error) {
		return wakeRestartReadiness{
			Prepared:     false,
			Record:       refusedRecord,
			RecordExists: true,
		}, nil
	}
	readCalls := 0
	wakeRestartReadRecord = func(int, *wakeAgentDir) (wakeRestartRecord, bool, error) {
		readCalls++
		return refusedRecord, true, nil
	}

	result, err := requestWakeRestart(fixture.root, fixture.agent)
	if err == nil {
		t.Fatalf("refused record must terminate with an error; got result=%#v", result)
	}
	if result.Status != "refused" {
		t.Fatalf("result.Status = %q, want %q (refused must not be swallowed into a timeout)", result.Status, "refused")
	}
	if !strings.Contains(result.Reason, refusedRecord.Reason) {
		t.Fatalf("result.Reason = %q, want it to contain the refused record reason %q", result.Reason, refusedRecord.Reason)
	}
	if readCalls == 0 {
		t.Fatalf("refused record reader was never called; the negative path was not exercised")
	}
	// Guard against a swallow-all implementation: the loop must NOT spin to the
	// timeout when the record is stably refused. readCalls stays small because
	// the first refused read terminates the loop.
	if readCalls > 4 {
		t.Fatalf("readiness loop spun %d times on a stably-refused record; refused should terminate immediately", readCalls)
	}
}
