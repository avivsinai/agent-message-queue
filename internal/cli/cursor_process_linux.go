//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cursorLinuxPIDs lists every live pid from /proc.
func cursorLinuxPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var pids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// processParentMap lists every live pid's parent pid from /proc/<pid>/stat.
func processParentMap() (map[int]int, error) {
	pids, err := cursorLinuxPIDs()
	if err != nil {
		return nil, err
	}
	parents := make(map[int]int, len(pids))
	for _, pid := range pids {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		if ppid, ok := linuxProcPPID(string(data)); ok {
			parents[pid] = ppid
		}
	}
	return parents, nil
}

// cursorNamedPIDs lists the candidate process ids (linux build).
func cursorNamedPIDs() ([]int, error) {
	return cursorLinuxPIDs()
}

// processCwd reads a process's working directory from /proc/<pid>/cwd.
func processCwd(pid int) (string, error) {
	path, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("cwd of pid %d is relative", pid)
	}
	return filepath.Clean(path), nil
}

// linuxProcPPID reads stat field 4 (ppid), after the comm field in parens,
// with the same parsing convention linuxProcIdentity uses.
func linuxProcPPID(stat string) (int, bool) {
	endComm := strings.LastIndex(stat, ")")
	if endComm < 0 || endComm+2 >= len(stat) {
		return 0, false
	}
	fields := strings.Fields(stat[endComm+2:])
	// fields[0] is stat field 3 (state); ppid is stat field 4.
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return 0, false
	}
	return ppid, true
}
