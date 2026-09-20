//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"
	"strconv"
	"strings"
)

// lifetimeOwnerPidOS reports the pid recorded in the entry's lifetime lock
// file (N3, review-b5: the exit-6 refusal must name the owner).
//
// review-828-r1 P1: a kernel-side discovery (fcntl F_GETLK) can never see
// the holder — the lifetime lock is taken with flock(2), a different lock
// namespace (measured live on darwin: a flock conflict reports l_pid=-1;
// on Linux the two namespaces are independent by documented design). So
// discovery is cooperative: acquireLifetimeLock writes its pid into the
// lock file body right after the flock succeeds, and this function reads
// it back. The recorded pid is best-effort text, NOT verified process
// identity: a failed truncate can leave old numeric text and a partial
// write can parse as a number, so it must never be used as authority to
// kill a process. The flock, not the content, is the ownership authority —
// a stale pid from a hard kill is harmless advisory text (the new up
// reclaims the phantom and truncates-rewrites the body on its own
// acquire). A missing, empty, or unparseable body reports 0 (unknown):
// never an error source.
func lifetimeOwnerPidOS(regPath, entryID string) int {
	lockPath := lockFilePath(regPath, entryID)
	body, err := os.ReadFile(lockPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
