// Package acp: explicit cleanup of amq-acp's remote-events state.
//
// amq-acp writes one record per inbound event under its state dir's
// remote-events/ directory (<event>.json, <event>.mailbox.json,
// <event>.mailbox.lock, <event>.cancelled). Nothing ever removes them. The
// repository rule is that cleanup is explicit, never automatic: there is no
// background sweeper, so the owner removes old records with
// `amq-acp cleanup`.
package acp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CleanupReport names the files a cleanup would or did touch, so the command
// prints exactly what it removed (or would remove) and what it left behind.
type CleanupReport struct {
	// Removed holds the base names of files removed, or that would be removed
	// when a dry run was requested.
	Removed []string
	// Skipped holds recent *.mailbox.lock files left in place because their
	// mtime is younger than the cutoff: the writer takes a flock on them for
	// the brief publish, so a fresh lock may still be held and is not removed.
	Skipped []string
}

// RemoteEventsDir resolves the amq-acp remote-events directory under the queue
// root, exactly as the server does: the state dir is AMQ_ACP_STATE_DIR when
// set (and kept under the root), else <root>/meta/acp. It refuses a resolved
// directory that is not under the root.
func RemoteEventsDir(root string) (string, error) {
	stateDir, err := resolveStateDir(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(stateDir, "remote-events")
	if !pathWithin(root, dir) {
		return "", contextError("refusing to clean a remote-events dir outside %s", EnvRoot)
	}
	return dir, nil
}

// CleanupRemoteEvents removes the regular files directly in dir whose mtime is
// older than cutoff. It never recurses, never follows symlinks (an entry is
// only touched when its Lstat says it is a regular file), and only ever names
// files that are a direct child of dir. A *.mailbox.lock whose mtime is
// younger than the cutoff is skipped and reported, because the writer's flock
// may still be held on it. When dryRun is true nothing is removed and Removed
// lists what would be removed.
func CleanupRemoteEvents(dir string, cutoff time.Time, dryRun bool) (CleanupReport, error) {
	var rep CleanupReport
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		full := filepath.Join(dir, name)
		info, err := os.Lstat(full)
		if err != nil {
			continue // vanished between listing and stat; leave it
		}
		if !info.Mode().IsRegular() {
			continue // symlinks, fifos, sockets, etc.: never touch
		}
		if !strictChild(dir, full) {
			continue // refuse anything that would resolve outside dir
		}
		if isMailboxLock(name) && !info.ModTime().Before(cutoff) {
			rep.Skipped = append(rep.Skipped, name)
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue // newer than the cutoff: keep
		}
		if !dryRun {
			if err := os.Remove(full); err != nil {
				return rep, err
			}
		}
		rep.Removed = append(rep.Removed, name)
	}
	sort.Strings(rep.Removed)
	sort.Strings(rep.Skipped)
	return rep, nil
}

// isMailboxLock reports that name is the event's publish lock file.
func isMailboxLock(name string) bool { return strings.HasSuffix(name, ".mailbox.lock") }

// strictChild reports that full is a direct child of dir (its relative path has
// no path separator). A defence so a crafted entry never resolves outside dir.
func strictChild(dir, full string) bool {
	rel, err := filepath.Rel(dir, full)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !strings.Contains(rel, string(filepath.Separator))
}
