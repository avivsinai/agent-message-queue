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

// staleInspection is what one pass of the stale harness check collected,
// including what it could NOT collect. A failed step is recorded as an
// error and the pass is never empty: advice must survive a failed ps or
// an unresolvable installed path.
type staleInspection struct {
	Probes   []staleProbe
	Path     string // the resolved installed amq-acp, when it resolved
	ListErr  error  // the process-list collection failed
	PathErr  error  // the installed amq-acp path could not be resolved
	IdentErr error  // the installed amq-acp identity could not be read
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

// staleACPInspect runs one full inspection pass under one total deadline,
// ps included: a single context bounds the whole pass, and every exec uses
// exec.CommandContext with WaitDelay, so a stalled child cannot extend it.
// A process whose identity cannot be read is unknown, not current. A
// failed step is recorded on the inspection, never swallowed: the caller
// reports it as advice, and partial results (complete candidate rows
// already parsed from a timed-out ps) are still checked.
func staleACPInspect(ctx context.Context) staleInspection {
	var insp staleInspection
	insp.Path, insp.PathErr = installedACPPath()
	procs, listErr := listACPProcesses(ctx)
	insp.ListErr = listErr
	installed, identErr := installedIdentity()
	insp.IdentErr = identErr
	for _, p := range procs {
		probe := staleProbe{PID: p.PID}
		switch {
		case insp.IdentErr != nil:
			probe.Reason = "the installed amq-acp could not be resolved"
		default:
			id, err := processIdentity(ctx, p.PID)
			switch {
			case err != nil:
				probe.Reason = err.Error()
			case !idSame(id, installed):
				probe.Stale = true
			}
		}
		insp.Probes = append(insp.Probes, probe)
	}
	return insp
}

// staleACPProbe is the deadline wrapper the direct callers use: it bounds
// one inspection pass with the probe's total deadline.
func staleACPProbe() ([]staleProbe, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	insp := staleACPInspect(ctx)
	if insp.IdentErr != nil && insp.ListErr == nil && len(insp.Probes) == 0 {
		return nil, insp.IdentErr
	}
	return insp.Probes, nil
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

// staleInspectionNotes adds the incomplete-inspection advice: one
// non-failing note per failed step, naming the step. It never invents
// PIDs and never turns an incomplete inspection into a doctor failure.
func staleInspectionNotes(insp staleInspection) []boundaryFailure {
	var notes []boundaryFailure
	if insp.ListErr != nil {
		notes = append(notes, boundaryFailure{
			Boundary: "stale_harness",
			Subject:  "process list",
			Detail:   fmt.Sprintf("could not list amq-acp processes: %v", insp.ListErr),
			Remedy:   "Re-run doctor; if it repeats, run `ps -axo pid=,comm=` and look for amq-acp",
		})
	}
	if insp.PathErr != nil {
		notes = append(notes, boundaryFailure{
			Boundary: "stale_harness",
			Subject:  "installed amq-acp",
			Detail:   fmt.Sprintf("could not resolve the installed amq-acp: %v", insp.PathErr),
			Remedy:   "Reinstall amq so the amq-acp companion sits beside amq-remote, then re-run doctor",
		})
	} else if insp.IdentErr != nil {
		notes = append(notes, boundaryFailure{
			Boundary: "stale_harness",
			Subject:  "installed amq-acp",
			Detail:   fmt.Sprintf("could not stat the installed amq-acp: %v", insp.IdentErr),
			Remedy:   "Reinstall amq so the amq-acp companion sits beside amq-remote, then re-run doctor",
		})
	}
	return notes
}

// noteStaleACP feeds the stale probe's advice into doctor's note closure.
// Both early and full doctor paths call it: a mailbox-only setup that never
// ran serve takes the early return, and that is exactly the setup that
// hits a stale harness. It never returns silently: a failed ps, an
// unresolvable installed amq-acp, or an unreadable identity each surface
// as one non-failing note, and the outcomes already collected are still
// reported. No doctor failure is ever added.
func noteStaleACP(note func(boundary, subject, detail, remedy string)) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	insp := staleACPInspect(ctx)
	path := insp.Path
	if insp.PathErr != nil {
		path = "the installed amq-acp"
	}
	for _, n := range staleInspectionNotes(insp) {
		note(n.Boundary, n.Subject, n.Detail, n.Remedy)
	}
	for _, n := range staleACPNotes(insp.Probes, path) {
		note(n.Boundary, n.Subject, n.Detail, n.Remedy)
	}
}
