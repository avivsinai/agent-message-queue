package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// bbn: after `brew upgrade amq`, a Buzz agent can keep running the old
// amq-acp, which answers "Not connected" with no hint. Doctor must name that
// exact process and say to Stop and Start the agent in Buzz Desktop.
func TestDoctorNamesAStaleACP(t *testing.T) {
	current := fakeACPList(t,
		filepath.Join(t.TempDir(), "Cellar", "amq", "0.84.0", "bin", "amq-acp"))

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
	if !strings.Contains(f.Detail, "Cellar") || !strings.Contains(f.Detail, current) {
		t.Fatalf("detail = %q, want the running path and the installed path %q", f.Detail, current)
	}
}

// bbn review: a mailbox-only setup that never ran `serve` takes doctor's
// early "no state directory" return; that is the setup that hits a stale
// harness, so the stale check must run on that path too.
func TestDoctorReportsStaleACPWithoutStateDir(t *testing.T) {
	root := t.TempDir() // no extensions/remote, so doctor returns early
	fakeACPList(t, "/opt/homebrew/Cellar/amq/0.84.0/bin/amq-acp")

	out, code, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	if code != protocol.ExitActionRequired || !doctorHasBoundary(out, "stale_harness", "pid 111") {
		t.Fatalf("early return missed stale_harness: exit=%d failing=%v", code, out.(map[string]any)["failing"])
	}
}

// fakeACPList replaces the listACPProcesses seam with one stale process
// (pid 111) and one current process (pid 222) and returns the installed
// path the current one must carry.
func fakeACPList(t *testing.T, stalePath string) string {
	t.Helper()
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
			{PID: 111, Path: stalePath},
			{PID: 222, Path: current},
		}, nil
	}
	return current
}
