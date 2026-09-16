//go:build darwin || linux

package hookinstall

import (
	"fmt"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group so that
// cmd.Cancel can kill the outer group (bash + watchdog). The reattach
// job creates its own group via set -m; killDescendants handles that.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the outer process group (bash + watchdog) and
// then recursively kills any descendant process groups that the script
// created via set -m (the reattach job). Process groups are not nested,
// so kill(-pgid) only covers the outer group; descendants in separate
// groups are found and killed individually.
func killProcessGroup(pid int) error {
	// Kill the outer process group first (bash + watchdog).
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	// Kill any orphaned descendant groups (reattach job from set -m).
	killDescendants(pid)
	return nil
}

// killDescendants finds and kills all descendant processes of the given
// PID recursively. This covers the reattach job's separate process group
// that kill(-pgid) cannot reach.
func killDescendants(rootPid int) {
	for _, pid := range findDescendants(rootPid) {
		// Kill the process group it leads (if any) and the process itself.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// findDescendants returns all descendant PIDs of rootPid using pgrep -P.
// pgrep is available on both Linux and macOS.
func findDescendants(rootPid int) []int {
	out, err := exec.Command("pgrep", "-P", fmt.Sprintf("%d", rootPid)).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range splitFields(string(out)) {
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil && pid > 0 {
			pids = append(pids, pid)
			// Recurse into children
			pids = append(pids, findDescendants(pid)...)
		}
	}
	return pids
}

// splitFields splits whitespace-separated fields.
func splitFields(s string) []string {
	var fields []string
	current := ""
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			if current != "" {
				fields = append(fields, current)
				current = ""
			}
		} else {
			current += string(r)
		}
	}
	if current != "" {
		fields = append(fields, current)
	}
	return fields
}
