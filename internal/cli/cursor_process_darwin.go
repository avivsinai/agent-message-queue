//go:build darwin

package cli

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// cursorDarwinPIDs lists every live pid from the kern.proc.all sysctl,
// walking the returned kinfo_proc array for P_pid.
func cursorDarwinPIDs() ([]int, error) {
	buf, err := syscall.Sysctl("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	const kinfoProcSize = int(unsafe.Sizeof(unix.KinfoProc{}))
	if len(buf) == 0 {
		return nil, nil
	}
	if len(buf)%kinfoProcSize != 0 {
		return nil, fmt.Errorf("list processes: %d bytes is not a kinfo_proc multiple", len(buf))
	}
	count := len(buf) / kinfoProcSize
	raw := []byte(buf)
	procs := unsafe.Slice((*unix.KinfoProc)(unsafe.Pointer(&raw[0])), count)
	pids := make([]int, 0, count)
	for i := range procs {
		if pid := int(procs[i].Proc.P_pid); pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// processCwd reads a process's working directory.
func processCwd(pid int) (string, error) {
	return darwinProcessCwd(pid)
}

// processParentMapDarwin reads every live pid's ppid from the same
// kern.proc.all array.
func processParentMapDarwin() (map[int]int, error) {
	buf, err := syscall.Sysctl("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	const kinfoProcSize = int(unsafe.Sizeof(unix.KinfoProc{}))
	if len(buf) == 0 {
		return nil, nil
	}
	if len(buf)%kinfoProcSize != 0 {
		return nil, fmt.Errorf("list processes: %d bytes is not a kinfo_proc multiple", len(buf))
	}
	count := len(buf) / kinfoProcSize
	raw := []byte(buf)
	procs := unsafe.Slice((*unix.KinfoProc)(unsafe.Pointer(&raw[0])), count)
	parents := make(map[int]int, count)
	for i := range procs {
		if pid := int(procs[i].Proc.P_pid); pid > 0 {
			parents[pid] = int(procs[i].Eproc.Ppid)
		}
	}
	return parents, nil
}

// processParentMap lists every live pid's parent pid (darwin build).
func processParentMap() (map[int]int, error) {
	return processParentMapDarwin()
}

// cursorNamedPIDs lists the candidate process ids (darwin build).
func cursorNamedPIDs() ([]int, error) {
	return cursorDarwinPIDs()
}
