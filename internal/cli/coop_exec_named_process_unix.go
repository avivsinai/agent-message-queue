//go:build darwin || linux

package cli

import (
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/harness"
)

// pidDescendsFromMap reports whether pid has rootPID in its ancestry,
// following the parent map until the root or a repeat.
func pidDescendsFromMap(pid, rootPID int, parents map[int]int) bool {
	if pid == rootPID {
		return true
	}
	seen := make(map[int]bool)
	for pid > 0 && !seen[pid] {
		seen[pid] = true
		pid = parents[pid]
		if pid == rootPID {
			return true
		}
	}
	return false
}

// cursorNamedProcessView implements coopNamedProcessView for the Cursor
// naming poll: it answers whether a Cursor CLI process other than ours and
// ours' descendants runs with the given working directory. It reads the
// process list once per call; a poll interval of seconds makes that cheap
// and no state is kept between calls.
type cursorNamedProcessView struct{}

// othersInDir reports whether a Cursor CLI process whose cwd is dir runs
// and is neither ours nor one of ours' descendants. A process whose cwd
// cannot be read is not evidence of another Cursor: a process can exit or
// refuse the read between listing and reading, and only a positive cwd
// match counts.
func (cursorNamedProcessView) othersInDir(dir string, ours int) (bool, error) {
	pids, err := cursorNamedPIDs()
	if err != nil {
		return false, err
	}
	parents, err := processParentMap()
	if err != nil {
		return false, fmt.Errorf("read process ancestry: %w", err)
	}
	for _, pid := range pids {
		if pid == ours || pidDescendsFromMap(pid, ours, parents) {
			continue
		}
		info := inspectWakeProcessPlatform(pid)
		if !info.Running {
			continue
		}
		if !cursorCLIExecutable(info) {
			continue
		}
		cwd, cwdErr := processCwd(pid)
		if cwdErr != nil || cwd == "" {
			continue
		}
		if cwd == dir {
			return true, nil
		}
	}
	return false, nil
}

// cursorCLIExecutable reports whether the inspected process runs a Cursor
// CLI executable, by the same basename table harness.ForExecutable uses for
// the launched binary.
func cursorCLIExecutable(info wakeProcessInfo) bool {
	name := info.ExecutablePath
	if name == "" {
		name = info.Executable
	}
	if name == "" {
		return false
	}
	facts, ok := harness.ForExecutable(name)
	return ok && facts.Name == harness.Cursor
}
