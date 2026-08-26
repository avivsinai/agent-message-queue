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
// ADVISORY WRITE rows whose maj:min:ino equals the lock file. lsof -t is only
// the fallback if /proc/locks cannot be inspected.
type platformWriterLockInspector struct {
	Runner CommandRunner
}

func (i platformWriterLockInspector) Held(ctx context.Context, path string) (bool, error) {
	held, err := procLocksHeld(path)
	if err == nil {
		return held, nil
	}
	return lsofLockHeld(ctx, i.runner(), path)
}

func (i platformWriterLockInspector) runner() CommandRunner {
	if i.Runner != nil {
		return i.Runner
	}
	return ExecRunner{}
}

func procLocksHeld(path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	want := fmt.Sprintf("%d:%d:%d", unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino)
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
