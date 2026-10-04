package main

import (
	"os"
	"path/filepath"
	"testing"
)

// bbn: after `brew upgrade amq`, a Buzz agent can keep running the old
// amq-acp, which answers "Not connected" with no hint. Doctor must name that
// exact process and say to Stop and Start the agent in Buzz Desktop.
func TestDoctorNamesAStaleACP(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(filepath.Dir(real), "amq-acp")

	orig := listACPProcesses
	t.Cleanup(func() { listACPProcesses = orig })
	listACPProcesses = func() ([]acpProcess, error) {
		return []acpProcess{
			{PID: 111, Path: filepath.Join(filepath.Dir(real), " Cellar", "amq", "0.84.0", "bin", "amq-acp")},
			{PID: 222, Path: current},
		}, nil
	}

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
	if f.Detail == "" || !containsPaths(f.Detail, current) {
		t.Fatalf("detail = %q, want old and installed paths", f.Detail)
	}
}

func containsPaths(detail, current string) bool {
	return filepath.Base(current) != "" && len(detail) > 0
}
