//go:build linux

package adapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// platformWriterLockInspector reads /proc/locks. On Linux, flock(2) and
// fcntl(F_GETLK) are independent lock types (except over NFS), and Codex's
// writer lock is an flock, so F_GETLK would always report idle. Match FLOCK
// ADVISORY WRITE rows whose maj:min:ino equals the lock file. Inspection
// errors fail closed; lsof is not a lock inspector (an open fd is not a held
// flock).
type platformWriterLockInspector struct{}

func (platformWriterLockInspector) Held(_ context.Context, path string) (bool, error) {
	return procLocksHeld(path)
}

func procLocksHeld(path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	// fs/locks.c prints "%02x:%02x:%lu" — hex major/minor, decimal inode.
	want := fmt.Sprintf("%02x:%02x:%d", unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino)
	f, err := os.Open("/proc/locks")
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		// Codex takes flock LOCK_EX, which /proc/locks reports as WRITE.
		// LOCK_SH is not an exclusive writer; do not treat it as a live seat.
		if fields[1] != "FLOCK" || fields[2] != "ADVISORY" || fields[3] != "WRITE" {
			continue
		}
		if fields[5] == want {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, err
	}
	return false, nil
}
