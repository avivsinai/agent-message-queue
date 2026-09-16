//go:build darwin || linux

package hookinstall

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setProcessGroup puts the command in its own process group so cleanup
// can target it specifically.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the outer process group (bash + watchdog).
// This is a bounded single-syscall operation.
func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// killReattachGroup kills the reattach job's separate process group
// (created by set -m). The PID is fixture-owned: the test binary writes
// its PID to a file before sleeping, giving us the exact group leader.
// This is a bounded single-syscall operation — no recursive scanner.
func killReattachGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// killOrphanedReattach is a last-resort fallback for the window where the
// reattach subshell has started (set -m gave it its own group) but the
// fixture binary hasn't published its PID yet. It runs a single-level
// (non-recursive) pgrep -P for direct children of the outer bash PID and
// kills each child's process group. This is bounded: one exec.Command, no
// recursion, and only runs on the failure path when fixture-owned identity
// is unavailable.
func killOrphanedReattach(outerPid int) {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(outerPid)).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Fields(string(out)) {
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}
