package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// acpProcess is one running amq-acp process with its launch name and start
// time.
type acpProcess struct {
	PID   int
	Name  string
	Start time.Time
}

// listACPProcesses lists running amq-acp processes with their start times.
// Tests replace this seam. Nothing is listed on Windows.
var listACPProcesses = listACPProcessesOS

// // installedACPPath resolves the amq-acp that the installed amq-remote ships
// with: amq-remote's own real path (symlinks resolved), with amq-acp beside
// it. The resolved sibling is itself resolved again, so a symlinked install
// still stats the real file.
func installedACPPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(real), "amq-acp"), nil
}

// installedTime is when the installed amq-acp was last replaced (the file's
// inode change time). Tests replace this seam.
var installedTime = installedTimeOS

// staleACPFailures names every running amq-acp that started before the
// installed amq-acp was last replaced: an upgrade cannot reach a process
// that predates it. A process whose start time cannot be read is skipped;
// doctor never guesses. argv[0] is not executable identity, so the decision
// is by time, not by path.
func staleACPFailures() ([]boundaryFailure, error) {
	replaced, err := installedTime()
	if err != nil {
		return nil, err
	}
	procs, err := listACPProcesses()
	if err != nil {
		return nil, err
	}
	var out []boundaryFailure
	for _, p := range procs {
		if p.Start.IsZero() || !p.Start.Before(replaced) {
			continue
		}
		out = append(out, boundaryFailure{
			Boundary: "stale_harness",
			Subject:  fmt.Sprintf("pid %d", p.PID),
			Detail: fmt.Sprintf("amq-acp pid %d started %s, before the installed amq-acp was replaced at %s",
				p.PID, p.Start.Format(time.RFC3339), replaced.Format(time.RFC3339)),
			Remedy: "If this is a Buzz Desktop agent, Stop and Start it so it runs the installed amq-acp",
		})
	}
	return out, nil
}

// failStaleACP feeds staleACPFailures into doctor's fail closure. Both early
// and full doctor paths call it: a mailbox-only setup that never ran serve
// takes the early return, and that is exactly the setup that hits a stale
// harness.
func failStaleACP(fail func(boundary, subject, detail, remedy string)) {
	stale, err := staleACPFailures()
	if err != nil {
		return
	}
	for _, f := range stale {
		fail(f.Boundary, f.Subject, f.Detail, f.Remedy)
	}
}
