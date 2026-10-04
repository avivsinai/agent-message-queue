//go:build unix

package main

import (
	"context"
	"testing"
	"time"
)

// bbn r4: comm is everything after the pid, so a launch path with spaces
// stays whole and basename matching still decides candidacy.
func TestACPCommWithSpacesIsKeptWhole(t *testing.T) {
	orig := listACPProcesses
	t.Cleanup(func() { listACPProcesses = orig })
	listACPProcesses = listACPProcessesOS
	origCLI := psListCLI
	t.Cleanup(func() { psListCLI = origCLI })
	psListCLI = func(ctx context.Context) ([]byte, error) {
		return []byte(
			"  111 /Applications/Agent Tools/amq-acp\n" +
				"  222 /tmp/amq-acp backup\n" +
				"  333 amq-acp\n" +
				"  444 /usr/libexec/logd\n"), nil
	}

	procs, err := listACPProcessesOS(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 2 {
		t.Fatalf("procs = %v, want the two amq-acp candidates only", procs)
	}
	if procs[0].PID != 111 || procs[0].Name != "/Applications/Agent Tools/amq-acp" {
		t.Fatalf("first = %+v, want the spaced path whole", procs[0])
	}
	if procs[1].PID != 333 || procs[1].Name != "amq-acp" {
		t.Fatalf("second = %+v, want the bare name", procs[1])
	}
}

// bbn r5: one TOTAL deadline bounds the whole probe, ps included. A ps that
// blocks until ctx.Done must be cut off at the bound; the probe returns
// within it, and the stalled child cannot extend it (WaitDelay covers the
// pipe close).
func TestStaleProbeReturnsWithinTotalDeadline(t *testing.T) {
	origList := listACPProcesses
	t.Cleanup(func() { listACPProcesses = origList })
	listACPProcesses = func(ctx context.Context) ([]acpProcess, error) {
		// Block until the probe's context fires: a stalled ps.
		<-ctx.Done()
		return nil, nil
	}
	origInst := installedIdentity
	t.Cleanup(func() { installedIdentity = origInst })
	installedIdentity = func() (execIdentity, error) { return testInstalled, nil }
	orig := probeTimeout
	t.Cleanup(func() { probeTimeout = orig })
	probeTimeout = 200 * time.Millisecond

	start := time.Now()
	probes, err := staleACPProbe()
	elapsed := time.Since(start)
	// ps was cut off by the context, so the candidate list is empty and the
	// probe returns a nil (not error) result well within the bound.
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 0 {
		t.Fatalf("probes = %v, want none when ps is cut off", probes)
	}
	if elapsed > probeTimeout+150*time.Millisecond {
		t.Fatalf("probe took %v, want within the %v total deadline (+wait margin)", elapsed, probeTimeout)
	}
}
