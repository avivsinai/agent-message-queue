//go:build unix

package main

import "testing"

// bbn r4: comm is everything after the pid, so a launch path with spaces
// stays whole and basename matching still decides candidacy.
func TestACPCommWithSpacesIsKeptWhole(t *testing.T) {
	orig := listACPProcesses
	t.Cleanup(func() { listACPProcesses = orig })
	listACPProcesses = listACPProcessesOS
	origCLI := psListCLI
	t.Cleanup(func() { psListCLI = origCLI })
	psListCLI = func() ([]byte, error) {
		return []byte(
			"  111 /Applications/Agent Tools/amq-acp\n" +
				"  222 /tmp/amq-acp backup\n" +
				"  333 amq-acp\n" +
				"  444 /usr/libexec/logd\n"), nil
	}

	procs, err := listACPProcessesOS()
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
