//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// psList runs ps with a C locale so the lstart date format is stable.
var psListCLI = func() ([]byte, error) {
	cmd := exec.Command("ps", "-axo", "pid=,lstart=,comm=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// listACPProcessesOS lists running amq-acp processes through ps, with each
// process's start time. Match by the basename of comm (argv[0]), so a bare
// name and a full path both match; argv[0] is not executable identity, the
// start time decides. A process whose start time cannot be parsed is
// skipped. A ps failure is not a doctor failure: doctor reports only what it
// can see.
func listACPProcessesOS() ([]acpProcess, error) {
	raw, err := psListCLI()
	if err != nil {
		return nil, nil
	}
	var out []acpProcess
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		// lstart is 5 fields: weekday month day time year. comm follows and
		// may itself contain spaces, so parse left to right with a fixed cut.
		if len(fields) < 7 || fields[0] == "" {
			continue
		}
		if !strings.EqualFold(filepath.Base(fields[6]), "amq-acp") {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		// time.ANSIC is exactly the LC_ALL=C lstart format.
		start, err := time.Parse(time.ANSIC, strings.Join(fields[1:6], " "))
		if err != nil {
			continue
		}
		out = append(out, acpProcess{PID: pid, Name: fields[6], Start: start})
	}
	return out, nil
}

// installedTimeOS is when the installed amq-acp was last replaced: the ctime
// of its resolved path. A file that cannot be stat'ed is no evidence.
func installedTimeOS() (time.Time, error) {
	path, err := installedACPPath()
	if err != nil {
		return time.Time{}, err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return time.Time{}, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return time.Time{}, err
	}
	return ctimeOf(info), nil
}
