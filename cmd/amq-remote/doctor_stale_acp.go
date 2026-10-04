package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// execIdentity is one executable's filesystem identity: its device and
// inode. Two processes run the same binary exactly when dev and inode
// match; a touch, a chmod, or a symlink retarget cannot forge that pair.
// A reader that cannot learn the device sets Dev to anyDevice and the
// comparison falls back to the inode alone.
type execIdentity struct {
	Dev   uint64
	Inode uint64
}

// anyDevice marks an identity read without device evidence.
const anyDevice = 0

// acpProcess is one running amq-acp candidate with its launch name.
type acpProcess struct {
	PID  int
	Name string
}

// listACPProcesses lists running amq-acp candidates by comm basename.
// Tests replace this seam. Nothing is listed on Windows.
var listACPProcesses = listACPProcessesOS

// processIdentity reads the executable identity of one running process.
// Tests replace this seam. A zero identity with an error means the identity
// could not be read; doctor never guesses.
var processIdentity = processIdentityOS

// installedACPPath resolves the amq-acp that the installed amq-remote ships
// with: amq-remote's own real path (symlinks resolved), with amq-acp beside
// it. The sibling is itself resolved again, so a symlinked install still
// stats the real file.
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

// staleACPFailures names every running amq-acp whose running executable
// differs from the installed one: an upgrade cannot reach a process that
// still runs the old binary. A process whose identity cannot be read is
// skipped; doctor never guesses.
func staleACPFailures() ([]boundaryFailure, error) {
	path, err := installedACPPath()
	if err != nil {
		return nil, err
	}
	installed, err := installedIdentity()
	if err != nil {
		return nil, err
	}
	procs, err := listACPProcesses()
	if err != nil {
		return nil, err
	}
	var out []boundaryFailure
	for _, p := range procs {
		id, err := processIdentity(p.PID)
		if err != nil {
			// No readable identity is no evidence; skip the process.
			continue
		}
		if idSame(id, installed) {
			continue
		}
		out = append(out, boundaryFailure{
			Boundary: "stale_harness",
			Subject:  fmt.Sprintf("pid %d", p.PID),
			Detail:   fmt.Sprintf("amq-acp pid %d runs a different executable than the installed %s; if this is a Buzz Desktop agent, Stop and Start it", p.PID, path),
			Remedy:   "If this is a Buzz Desktop agent, Stop and Start it so it runs the installed amq-acp",
		})
	}
	return out, nil
}

func idSame(a, b execIdentity) bool {
	if a.Dev == anyDevice || b.Dev == anyDevice {
		// One side had no device evidence; inode decides alone.
		return a.Inode == b.Inode
	}
	return a.Dev == b.Dev && a.Inode == b.Inode
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
