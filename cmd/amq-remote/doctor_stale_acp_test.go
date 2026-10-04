package main

import (
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// fakeACPProcess replaces the listACPProcesses and installedTime seams: the
// installed amq-acp was replaced at installedAt, and the fake list carries
// one process launched at startedBefore (stale) and one at startedAfter
// (current). A bare-name launch is included to prove argv[0] matching.
func fakeACPList(t *testing.T, installedAt, startedBefore, startedAfter time.Time) {
	t.Helper()
	origList, origTime := listACPProcesses, installedTime
	t.Cleanup(func() { listACPProcesses, installedTime = origList, origTime })
	listACPProcesses = func() ([]acpProcess, error) {
		return []acpProcess{
			{PID: 111, Name: "/opt/homebrew/Cellar/amq/0.84.0/bin/amq-acp", Start: startedBefore},
			{PID: 222, Name: "amq-acp", Start: startedAfter},
		}, nil
	}
	installedTime = func() (time.Time, error) { return installedAt, nil }
}

// bbn: after `brew upgrade amq`, a Buzz agent can keep running the old
// amq-acp, which answers "Not connected" with no hint. Doctor must name that
// exact process and say to Stop and Start the agent in Buzz Desktop.
func TestDoctorNamesAStaleACP(t *testing.T) {
	installedAt := time.Now()
	fakeACPList(t, installedAt, installedAt.Add(-time.Hour), installedAt.Add(time.Hour))

	failures, err := staleACPFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %d (%v), want exactly 1", len(failures), failures)
	}
	f := failures[0]
	if f.Boundary != "stale_harness" || f.Subject != "pid 111" || f.Remedy == "" {
		t.Fatalf("failure = %+v, want stale_harness/pid 111 with a remedy", f)
	}
	// The bare-name process (pid 222) started after the install and must not
	// be named: argv[0] is not executable identity, start time decides.
	if !strings.Contains(f.Detail, "before the installed amq-acp was replaced") {
		t.Fatalf("detail = %q, want both timestamps", f.Detail)
	}
}

// bbn review: a mailbox-only setup that never ran `serve` takes doctor's
// early "no state directory" return; that is the setup that hits a stale
// harness, so the stale check must run on that path too.
func TestDoctorReportsStaleACPWithoutStateDir(t *testing.T) {
	root := t.TempDir() // no extensions/remote, so doctor returns early
	fakeACPList(t, time.Now().Add(-time.Minute), time.Now().Add(-2*time.Hour), time.Now())

	out, code, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	if code != protocol.ExitActionRequired || !doctorHasBoundary(out, "stale_harness", "pid 111") {
		t.Fatalf("early return missed stale_harness: exit=%d failing=%v", code, out.(map[string]any)["failing"])
	}
}
