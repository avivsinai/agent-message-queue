//go:build unix

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// listACPProcessesOS lists running amq-acp processes through ps. Only the
// executable path is read; a process whose path cannot be read is skipped.
func listACPProcessesOS() ([]acpProcess, error) {
	raw, err := exec.Command("ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		// No process list is not a doctor failure; doctor reports only what
		// it can see.
		return nil, nil
	}
	var out []acpProcess
	for _, line := range strings.Split(string(raw), "\n") {
		pidStr, comm, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.HasSuffix(comm, "/amq-acp") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
		if err != nil {
			continue
		}
		out = append(out, acpProcess{PID: pid, Path: comm})
	}
	return out, nil
}
