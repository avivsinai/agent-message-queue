package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// execIdentity is one executable's filesystem identity: its device and
// inode. Two processes run the same binary exactly when dev and inode
// match; a touch, a chmod, or a symlink retarget cannot forge that pair.
type execIdentity struct {
	Dev   uint64
	Inode uint64
}

// acpProcess is one running amq-acp candidate with its launch name.
type acpProcess struct {
	PID  int
	Name string
}

// staleProbe is the outcome of checking one candidate process: it is
// stale when it runs a different executable, or unknown when its identity
// could not be read. Unknown is not current: it never counts as fine and
// is reported, never guessed.
type staleProbe struct {
	PID    int
	Stale  bool
	Reason string // set when the identity could not be read
}

// probeTimeout is the ONE total deadline for the whole stale probe, ps
// included. Tests shorten it.
var probeTimeout = 3 * time.Second

// childWaitDelay bounds how long Output may wait to close the pipes after
// a child's deadline, so a stalled child holding the pipe cannot block it.
const childWaitDelay = 500 * time.Millisecond

// listACPProcesses lists running amq-acp candidates by comm basename.
// Tests replace this seam. Nothing is listed on Windows.
var listACPProcesses = listACPProcessesOS

// processIdentity reads the executable identity of one running process.
// Tests replace this seam. An error means unknown, not current.
var processIdentity = processIdentityOS

// installedACPPath resolves the amq-acp that the installed amq-remote ships
// with: amq-remote's own real path (symlinks resolved), with amq-acp beside
// it.
var installedACPPath = installedACPPathFn

func installedACPPathFn() (string, error) {
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

// installedIdentity is the (dev, inode) of the resolved installed amq-acp.
var installedIdentity = installedIdentityOS

func idSame(a, b execIdentity) bool {
	return a.Dev == b.Dev && a.Inode == b.Inode
}

// staleACPProbe checks every running amq-acp against the installed one
// under one total deadline, ps included: a single context bounds the whole
// probe, and every exec uses exec.CommandContext with WaitDelay, so a
// stalled child cannot extend the probe. A process whose identity cannot
// be read is unknown, not current, and is reported by the caller.
func staleACPProbe() ([]staleProbe, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	installed, err := installedIdentity()
	if err != nil {
		return nil, err
	}
	procs, err := listACPProcesses(ctx)
	if err != nil {
		return nil, err
	}
	var out []staleProbe
	for _, p := range procs {
		probe := staleProbe{PID: p.PID}
		id, err := processIdentity(ctx, p.PID)
		switch {
		case err != nil:
			probe.Reason = err.Error()
		case !idSame(id, installed):
			probe.Stale = true
		}
		out = append(out, probe)
	}
	return out, nil
}

// staleACPNotes turns probe outcomes into advice notes: one per stale
// process (naming it and the installed path, with the Stop-and-Start
// remedy) and, when anything is unknown, one note per distinct reason
// naming every affected pid. Stale is advice, not a failure: a different
// identity can also be a byte-identical copy or another binary with the
// same name, so the operator decides.
func staleACPNotes(probes []staleProbe, path string) []boundaryFailure {
	var notes []boundaryFailure
	reasonPids := map[string][]int{}
	for _, p := range probes {
		switch {
		case p.Stale:
			notes = append(notes, boundaryFailure{
				Boundary: "stale_harness",
				Subject:  fmt.Sprintf("pid %d", p.PID),
				Detail:   fmt.Sprintf("amq-acp pid %d runs a different executable than the installed %s; if this is a Buzz Desktop agent, Stop and Start it", p.PID, path),
				Remedy:   "If this is a Buzz Desktop agent, Stop and Start it so it runs the installed amq-acp",
			})
		case p.Reason != "":
			reasonPids[p.Reason] = append(reasonPids[p.Reason], p.PID)
		}
	}
	reasons := make([]string, 0, len(reasonPids))
	for reason := range reasonPids {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		pids := reasonPids[reason]
		parts := make([]string, len(pids))
		for i, pid := range pids {
			parts[i] = strconv.Itoa(pid)
		}
		notes = append(notes, boundaryFailure{
			Boundary: "stale_harness",
			Subject:  "pid(s) " + strings.Join(parts, ", "),
			Detail:   fmt.Sprintf("could not check amq-acp pid(s) %s: %s", strings.Join(parts, ", "), reason),
			Remedy:   "If this is a Buzz Desktop agent, Stop and Start it and re-run doctor",
		})
	}
	return notes
}

// noteStaleACP feeds the stale probe's advice into doctor's note closure.
// Both early and full doctor paths call it: a mailbox-only setup that never
// ran serve takes the early return, and that is exactly the setup that
// hits a stale harness.
func noteStaleACP(note func(boundary, subject, detail, remedy string)) {
	probes, err := staleACPProbe()
	if err != nil {
		return
	}
	path, err := installedACPPath()
	if err != nil {
		return
	}
	for _, n := range staleACPNotes(probes, path) {
		note(n.Boundary, n.Subject, n.Detail, n.Remedy)
	}
}
