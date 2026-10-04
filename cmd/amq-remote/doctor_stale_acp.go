package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// acpProcess is one running amq-acp process with its executable path.
type acpProcess struct {
	PID  int
	Path string
}

// listACPProcesses lists running amq-acp processes with their executable
// paths. Tests replace this seam. Nothing is listed on Windows.
var listACPProcesses = listACPProcessesOS

// installedACPPath resolves the amq-acp that the installed amq-remote ships
// with: amq-remote's own real path (symlinks resolved), with amq-acp beside
// it.
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

// staleACPFailures names every running amq-acp whose executable is not the
// installed one. A process whose path cannot be read is skipped; doctor
// never guesses.
func staleACPFailures() ([]boundaryFailure, error) {
	current, err := installedACPPath()
	if err != nil {
		return nil, err
	}
	procs, err := listACPProcesses()
	if err != nil {
		return nil, err
	}
	var out []boundaryFailure
	for _, p := range procs {
		if p.Path == "" || p.Path == current {
			continue
		}
		out = append(out, boundaryFailure{
			Boundary: "stale_harness",
			Subject:  fmt.Sprintf("pid %d", p.PID),
			Detail:   fmt.Sprintf("amq-acp %s is older than the installed %s", p.Path, current),
			Remedy:   "Stop and Start this agent in Buzz Desktop so it runs the installed amq-acp",
		})
	}
	return out, nil
}
