//go:build unix

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeACPList replaces the identity seams: the installed amq-acp has
// identity installed, and the candidate list carries three processes — one
// running a different executable (stale), one running the installed binary
// through a symlink (current), one bare-name candidate whose identity read
// fails (unknown).
func fakeACPList(t *testing.T, installed execIdentity) {
	t.Helper()
	origList, origID, origInst := listACPProcesses, processIdentity, installedIdentity
	t.Cleanup(func() {
		listACPProcesses, processIdentity, installedIdentity = origList, origID, origInst
	})
	listACPProcesses = func(ctx context.Context) ([]acpProcess, error) {
		return []acpProcess{
			{PID: 111, Name: "/opt/homebrew/Cellar/amq/0.84.0/bin/amq-acp"},
			{PID: 222, Name: "/opt/homebrew/bin/amq-acp"},
			{PID: 333, Name: "amq-acp"},
		}, nil
	}
	processIdentity = func(ctx context.Context, pid int) (execIdentity, error) {
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

var testInstalled = execIdentity{Dev: 16777232, Inode: 183321562}

// bbn: after `brew upgrade amq`, a Buzz agent can keep running the old
// amq-acp, which answers "Not connected" with no hint. Doctor must name that
// exact process and say to Stop and Start the agent in Buzz Desktop.
func TestDoctorNamesAStaleACP(t *testing.T) {
	fakeACPList(t, testInstalled)

	probes, err := staleACPProbe()
	if err != nil {
		t.Fatal(err)
	}
	notes := staleACPNotes(probes, "/opt/homebrew/bin/amq-acp")
	if len(notes) != 2 {
		t.Fatalf("notes = %d (%v), want a stale note and an unknown note", len(notes), notes)
	}
	var stale, unknown *boundaryFailure
	for i := range notes {
		switch notes[i].Subject {
		case "pid 111":
			stale = &notes[i]
		case "pid(s) 333":
			unknown = &notes[i]
		}
	}
	if stale == nil || stale.Boundary != "stale_harness" || stale.Remedy == "" {
		t.Fatalf("stale note = %+v, want stale_harness/pid 111 with a remedy", stale)
	}
	if !strings.Contains(stale.Detail, "runs a different executable") {
		t.Fatalf("stale detail = %q, want the executable-identity wording", stale.Detail)
	}
	if unknown == nil || !strings.Contains(unknown.Detail, "could not check amq-acp pid(s) 333") {
		t.Fatalf("unknown note = %+v, want it to name pid 333", unknown)
	}
}

// bbn r5: an unreadable or timed-out process is unknown, not current, and a
// stale process must never just vanish. One note names every affected pid
// with the reason.
func TestUnknownProcessesAreReportedNotGuessed(t *testing.T) {
	fakeACPList(t, testInstalled)

	probes, err := staleACPProbe()
	if err != nil {
		t.Fatal(err)
	}
	var unknowns int
	for _, p := range probes {
		switch {
		case p.Stale:
			if p.PID != 111 {
				t.Fatalf("stale probe = %+v, want pid 111 only", p)
			}
		case p.Reason != "":
			unknowns++
			if p.PID != 333 {
				t.Fatalf("unknown probe = %+v, want pid 333 only", p)
			}
		}
	}
	if unknowns != 1 {
		t.Fatalf("unknown probes = %d, want exactly 1", unknowns)
	}
}

// bbn r5: same inode but a different device means a different executable.
func TestSameInodeDifferentDevIsStale(t *testing.T) {
	installed := execIdentity{Dev: 16777232, Inode: 183321562}
	origList, origID, origInst := listACPProcesses, processIdentity, installedIdentity
	t.Cleanup(func() {
		listACPProcesses, processIdentity, installedIdentity = origList, origID, origInst
	})
	listACPProcesses = func(ctx context.Context) ([]acpProcess, error) {
		return []acpProcess{{PID: 222, Name: "/opt/homebrew/bin/amq-acp"}}, nil
	}
	// pid 222 runs a file with the SAME inode but a DIFFERENT device: not
	// the installed binary.
	processIdentity = func(ctx context.Context, pid int) (execIdentity, error) {
		return execIdentity{Dev: installed.Dev + 1000, Inode: installed.Inode}, nil
	}
	installedIdentity = func() (execIdentity, error) { return installed, nil }

	probes, err := staleACPProbe()
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 || !probes[0].Stale || probes[0].Reason != "" {
		t.Fatalf("probes = %v, want pid 222 stale (same inode, different dev)", probes)
	}
}

// bbn r5: a mailbox-only setup that never ran `serve` takes doctor's
// early "no state directory" return; that is the setup that hits a stale
// harness, so the stale check must run on that path too.
func TestDoctorReportsStaleACPWithoutStateDir(t *testing.T) {
	root := t.TempDir() // no extensions/remote, so doctor returns early
	fakeACPList(t, testInstalled)

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

// collect noteStaleACP's output on one doctor path: the stale_harness
// notes and whether any doctor FAILURE was added. Every r6 regression
// asserts both: the advice must appear, and no failure may be added.
func noteStaleACPOnDoctor(t *testing.T) (notes, failures []boundaryFailure) {
	t.Helper()
	root := t.TempDir() // no extensions/remote, so doctor takes the early path
	out, _, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := out.(map[string]any)
	return toFailures(report["notes"]), toFailures(report["failing"])
}

// bbn r6: a ps that FAILS must not make doctor's stale advice vanish.
// The "could not list" note names the failed step, no PID is invented,
// and no doctor failure is added.
func TestNoteStaleACPReportsAFailedPS(t *testing.T) {
	origList, origID, origInst := listACPProcesses, processIdentity, installedIdentity
	t.Cleanup(func() { listACPProcesses, processIdentity, installedIdentity = origList, origID, origInst })
	listACPProcesses = func(ctx context.Context) ([]acpProcess, error) {
		return nil, errors.New("ps exited 1: permission denied")
	}
	installedIdentity = func() (execIdentity, error) { return testInstalled, nil }

	notes, failures := noteStaleACPOnDoctor(t)
	for _, f := range failures {
		if f.Boundary == "stale_harness" {
			t.Fatalf("failed ps must not add a doctor failure: %v", f)
		}
	}
	for _, n := range notes {
		if n.Boundary == "stale_harness" && strings.Contains(n.Detail, "could not list amq-acp processes: ps exited 1") {
			if strings.Contains(n.Detail, "pid") {
				t.Fatalf("collection note must not invent pids: %v", n)
			}
			return
		}
	}
	t.Fatalf("failed ps vanished from advice: notes=%v", notes)
}

// bbn r6: a ps that TIMES OUT after printing one complete candidate row
// must report BOTH the candidate's outcome (unknown here, since its
// identity cannot be read) and the collection note. The pro reviewer
// reproduced exactly this: latency fix worked, reporting did not.
func TestNoteStaleACPReportsPartialRowsFromATimedOutPS(t *testing.T) {
	origList, origID, origInst := listACPProcesses, processIdentity, installedIdentity
	t.Cleanup(func() { listACPProcesses, processIdentity, installedIdentity = origList, origID, origInst })
	listACPProcesses = func(ctx context.Context) ([]acpProcess, error) {
		// One complete row was parsed from partial stdout before the stall.
		return []acpProcess{{PID: 444, Name: "/opt/homebrew/bin/amq-acp"}}, context.DeadlineExceeded
	}
	processIdentity = func(ctx context.Context, pid int) (execIdentity, error) {
		return execIdentity{}, errors.New("lsof timed out")
	}
	installedIdentity = func() (execIdentity, error) { return testInstalled, nil }

	notes, failures := noteStaleACPOnDoctor(t)
	for _, f := range failures {
		if f.Boundary == "stale_harness" {
			t.Fatalf("timed-out ps must not add a doctor failure: %v", f)
		}
	}
	var candidate, collection bool
	for _, n := range notes {
		switch {
		case n.Boundary == "stale_harness" && strings.Contains(n.Detail, "could not check amq-acp pid(s) 444"):
			candidate = true
		case n.Boundary == "stale_harness" && strings.Contains(n.Detail, "could not list amq-acp processes"):
			collection = true
		}
	}
	if !candidate || !collection {
		t.Fatalf("timed-out ps: candidate=%v collection=%v notes=%v", candidate, collection, notes)
	}
}

// bbn r6: when installedACPPath fails, its note appears, the collected
// outcomes are still reported (as unknown, never silently dropped), and no
// doctor failure is added. No second successful lookup is required.
func TestNoteStaleACPReportsAnUnresolvableInstalledPath(t *testing.T) {
	origList, origID, origInst := listACPProcesses, processIdentity, installedACPPath
	t.Cleanup(func() { listACPProcesses, processIdentity, installedACPPath = origList, origID, origInst })
	listACPProcesses = func(ctx context.Context) ([]acpProcess, error) {
		return []acpProcess{{PID: 555, Name: "/opt/homebrew/bin/amq-acp"}}, nil
	}
	processIdentity = func(ctx context.Context, pid int) (execIdentity, error) {
		return execIdentity{}, errors.New("unreadable")
	}
	installedACPPath = func() (string, error) { return "", errors.New("os.Executable failed") }

	notes, failures := noteStaleACPOnDoctor(t)
	for _, f := range failures {
		if f.Boundary == "stale_harness" {
			t.Fatalf("unresolvable installed path must not add a doctor failure: %v", f)
		}
	}
	var path, outcome bool
	for _, n := range notes {
		switch {
		case n.Boundary == "stale_harness" && strings.Contains(n.Detail, "could not resolve the installed amq-acp: os.Executable failed"):
			path = true
		case n.Boundary == "stale_harness" && strings.Contains(n.Detail, "could not check amq-acp pid(s) 555"):
			outcome = true
		}
	}
	if !path || !outcome {
		t.Fatalf("unresolvable installed path: path=%v outcome=%v notes=%v", path, outcome, notes)
	}
}
