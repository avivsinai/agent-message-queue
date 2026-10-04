package main

import (
	"errors"
	"strings"
	"testing"
)

// fakeACPList replaces the identity seams: the installed amq-acp has
// identity installed, and the candidate list carries three processes — one
// running a different executable (stale), one running the installed binary
// through a symlink (current), one bare-name candidate whose identity read
// fails (skipped).
func fakeACPList(t *testing.T, installed execIdentity) {
	t.Helper()
	origList, origID, origInst := listACPProcesses, processIdentity, installedIdentity
	t.Cleanup(func() {
		listACPProcesses, processIdentity, installedIdentity = origList, origID, origInst
	})
	listACPProcesses = func() ([]acpProcess, error) {
		return []acpProcess{
			{PID: 111, Name: "/opt/homebrew/Cellar/amq/0.84.0/bin/amq-acp"},
			{PID: 222, Name: "/opt/homebrew/bin/amq-acp"},
			{PID: 333, Name: "amq-acp"},
		}, nil
	}
	processIdentity = func(pid int) (execIdentity, error) {
		switch pid {
		case 111:
			return execIdentity{Dev: installed.Dev + 1, Inode: installed.Inode + 1}, nil
		case 222:
			return installed, nil
		default:
			return execIdentity{}, errors.New("unreadable")
		}
	}
	installedIdentity = func() (execIdentity, error) { return installed, nil }
}

// bbn: after `brew upgrade amq`, a Buzz agent can keep running the old
// amq-acp, which answers "Not connected" with no hint. Doctor must name that
// exact process and say to Stop and Start the agent in Buzz Desktop.
func TestDoctorNamesAStaleACP(t *testing.T) {
	fakeACPList(t, execIdentity{Dev: 16777232, Inode: 183321562})

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
	if !strings.Contains(f.Detail, "runs a different executable") {
		t.Fatalf("detail = %q, want the executable-identity wording", f.Detail)
	}
}

// bbn review: a mailbox-only setup that never ran `serve` takes doctor's
// early "no state directory" return; that is the setup that hits a stale
// harness, so the stale check must run on that path too.
func TestDoctorReportsStaleACPWithoutStateDir(t *testing.T) {
	root := t.TempDir() // no extensions/remote, so doctor returns early
	fakeACPList(t, execIdentity{Dev: 16777232, Inode: 183321562})

	out, _, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	// stale_harness is advice now: it must appear in notes, never in
	// failing. The exit code stays whatever the real failures (the endpoint
	// failure here) make it; the note itself must not add one.
	report, _ := out.(map[string]any)
	for _, f := range toFailures(report["failing"]) {
		if f.Boundary == "stale_harness" {
			t.Fatalf("stale_harness must not be a failure: %v", f)
		}
	}
	found := false
	for _, n := range toFailures(report["notes"]) {
		if n.Boundary == "stale_harness" && n.Subject == "pid 111" {
			found = true
		}
	}
	if !found {
		t.Fatalf("early return missed the stale_harness note: notes=%v", report["notes"])
	}
}

func toFailures(v any) []boundaryFailure {
	fs, _ := v.([]boundaryFailure)
	return fs
}
