//go:build darwin || linux

package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"golang.org/x/sys/unix"
)

// A Claude Code agent can show several dialogs at once: the main thread's
// and each subagent's. Each open dialog is one entry file in the directory
// <agent>/.wake.dialog/, named for its key (session_id, agent_id or "main",
// tool_use_id) and holding its marker. The doorbell is held while any entry
// holds, and each entry closes only by its own facts. Every other writer
// keeps the single-file marker <agent>/.wake.dialog, and so does a Claude
// Code open while the agent's wake reads only that form
// (wakeReadsDialogEntries). When a Claude Code
// dialog opens over a single file, that file becomes the directory's entry
// legacy.json in one exchange, so the path is never absent, and it keeps the
// single-file rules. The directory goes when its last entry goes, so the
// hook gate that tests the path still skips amq when nothing is open. All
// changes hold the marker lock; entries are read without following a
// symlink and must be regular 0600 files.

const (
	// wakeDialogLegacyEntry is the entry that holds a single-file marker
	// inside the directory; it keeps the single-file open and close rules.
	wakeDialogLegacyEntry = "legacy.json"
	// maxWakeDialogEntries bounds a directory read; more reads as unknown.
	maxWakeDialogEntries = 64
)

// wakeDialogEntry is one marker of the agent's dialog state. name is "" for
// the single-file marker.
type wakeDialogEntry struct {
	name    string
	raw     []byte
	info    os.FileInfo
	readErr error
}

var wakeDialogKeyedEntryRe = regexp.MustCompile(`^[0-9a-f]{32}\.json$`)

// wakeDialogKeyedEntry reports an entry a Claude Code dialog open wrote.
func wakeDialogKeyedEntry(name string) bool {
	return wakeDialogKeyedEntryRe.MatchString(name)
}

// wakeDialogEntryName is the entry file of one dialog's key.
func wakeDialogEntryName(sessionID, agentID, toolUseID string) string {
	if agentID == "" {
		agentID = "main"
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + agentID + "\x00" + toolUseID))
	return hex.EncodeToString(sum[:16]) + ".json"
}

// openWakeDialogEntriesAt opens .wake.dialog when it is a directory of
// entries; ok is false when it is absent or a file.
func openWakeDialogEntriesAt(dirfd int, base string) (dir *os.File, ok bool, err error) {
	fd, err := unix.Openat(dirfd, wakeDialogFileName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		return os.NewFile(uintptr(fd), filepath.Join(base, wakeDialogFileName)), true, nil
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR):
		return nil, false, nil
	case errors.Is(err, unix.ELOOP):
		return nil, false, fmt.Errorf("wake dialog marker %s must not be a symlink", filepath.Join(base, wakeDialogFileName))
	}
	return nil, false, fmt.Errorf("open wake dialog entries: %w", err)
}

// wakeDialogEntryNames lists the entries of dir, sorted, without temp files.
func wakeDialogEntryNames(dir *os.File) ([]string, error) {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list wake dialog entries: %w", err)
	}
	kept := names[:0]
	for _, name := range names {
		if !strings.HasPrefix(name, ".") {
			kept = append(kept, name)
		}
	}
	if len(kept) > maxWakeDialogEntries {
		return nil, fmt.Errorf("%d wake dialog entries, more than %d", len(kept), maxWakeDialogEntries)
	}
	sort.Strings(kept)
	return kept, nil
}

// readWakeDialogEntriesInDir reads every marker through the canonical agent
// directory. err reports a directory that is no longer canonical; an entry's
// readErr a marker that cannot be read.
func readWakeDialogEntriesInDir(agentDir *wakeAgentDir) ([]wakeDialogEntry, error) {
	if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
		return nil, err
	}
	var entries []wakeDialogEntry
	err := agentDir.withFD(func(dirfd int) error {
		entries = readWakeDialogEntriesAt(dirfd, agentDir.path)
		return nil
	})
	return entries, err
}

func readWakeDialogEntriesAt(dirfd int, base string) []wakeDialogEntry {
	dir, ok, err := openWakeDialogEntriesAt(dirfd, base)
	if err != nil {
		return []wakeDialogEntry{{readErr: err}}
	}
	if !ok {
		raw, info, exists, readErr := readWakeRepairMetadataAt(dirfd, wakeDialogFileName, "wake dialog marker",
			filepath.Join(base, wakeDialogFileName), maxWakeMetadataFileBytes)
		if !exists {
			return nil
		}
		return []wakeDialogEntry{{raw: raw, info: info, readErr: readErr}}
	}
	defer func() { _ = dir.Close() }()
	names, err := wakeDialogEntryNames(dir)
	if err != nil {
		return []wakeDialogEntry{{name: wakeDialogFileName, readErr: err}}
	}
	entries := make([]wakeDialogEntry, 0, len(names))
	for _, name := range names {
		raw, info, exists, readErr := readWakeRepairMetadataAt(int(dir.Fd()), name, "wake dialog entry",
			filepath.Join(dir.Name(), name), maxWakeMetadataFileBytes)
		if exists {
			entries = append(entries, wakeDialogEntry{name: name, raw: raw, info: info, readErr: readErr})
		}
	}
	return entries
}

// removeWakeDialogEntriesAt removes, under the caller's marker lock, each
// readable marker remove accepts: the single file, or entries of the
// directory, which goes once it is empty. An unreadable entry stays.
func removeWakeDialogEntriesAt(dirfd int, agentDir *wakeAgentDir, remove func(name string, raw []byte, info os.FileInfo) bool) error {
	dir, ok, err := openWakeDialogEntriesAt(dirfd, agentDir.path)
	if err != nil {
		return err
	}
	if !ok {
		raw, info, exists, err := readWakeRepairMetadataAt(dirfd, wakeDialogFileName, "wake dialog marker",
			filepath.Join(agentDir.path, wakeDialogFileName), maxWakeMetadataFileBytes)
		if err != nil || !exists || !remove("", raw, info) {
			return err
		}
		if err := wakeUnlinkAt(dirfd, wakeDialogFileName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	}
	defer func() { _ = dir.Close() }()
	names, err := wakeDialogEntryNames(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		raw, info, exists, err := readWakeRepairMetadataAt(int(dir.Fd()), name, "wake dialog entry",
			filepath.Join(dir.Name(), name), maxWakeMetadataFileBytes)
		if err != nil || !exists || !remove(name, raw, info) {
			continue
		}
		if err := wakeUnlinkAt(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return removeEmptyWakeDialogEntriesAt(dirfd)
}

// removeEmptyWakeDialogEntriesAt removes the entries directory when it is
// empty; a directory with entries stays.
func removeEmptyWakeDialogEntriesAt(dirfd int) error {
	err := wakeUnlinkAt(dirfd, wakeDialogFileName, unix.AT_REMOVEDIR)
	if err == nil || errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) ||
		errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return nil
	}
	return err
}

// removeWakeDialogEntryVersion removes the marker the wake read as entry,
// only if it is still that version (same name, bytes, mtime and size), under
// the marker lock taken without waiting. It reports whether it removed it.
func removeWakeDialogEntryVersion(agentDir *wakeAgentDir, entry wakeDialogEntry) bool {
	if validateCanonicalWakeAgentDir(agentDir) != nil || entry.info == nil {
		return false
	}
	removed := false
	err := agentDir.withFD(func(dirfd int) error {
		return lockWakeDialogMarker(dirfd, unix.LOCK_EX|unix.LOCK_NB, func() error {
			return removeWakeDialogEntriesAt(dirfd, agentDir, func(name string, raw []byte, info os.FileInfo) bool {
				if name != entry.name || !bytes.Equal(raw, entry.raw) ||
					!info.ModTime().Equal(entry.info.ModTime()) || info.Size() != entry.info.Size() {
					return false
				}
				removed = true
				return true
			})
		})
	})
	return err == nil && removed
}

// wakeDialogTempName is a fresh name that entry listings skip.
func wakeDialogTempName(kind string) (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate wake dialog temp name: %w", err)
	}
	return fmt.Sprintf(".%s.%s.tmp.%d.%s", strings.TrimLeft(wakeDialogFileName, "."), kind, os.Getpid(), hex.EncodeToString(nonce[:])), nil
}

// writeWakeDialogEntryFileAt installs data as name in dirfd by a temp file
// and a rename; a non-zero modTime dates it.
func writeWakeDialogEntryFileAt(dirfd int, name string, data []byte, modTime time.Time) error {
	const label = "wake dialog entry"
	temp, err := wakeDialogTempName("entry")
	if err != nil {
		return err
	}
	fd, err := unix.Openat(dirfd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s temp: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), temp)
	tempPresent := true
	defer func() {
		_ = file.Close()
		if tempPresent {
			_ = wakeUnlinkAt(dirfd, temp, 0)
		}
	}()
	if n, err := file.Write(data); err != nil {
		return fmt.Errorf("write %s temp: %w", label, err)
	} else if n != len(data) {
		return fmt.Errorf("write %s temp: %w", label, io.ErrShortWrite)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s temp: %w", label, err)
	}
	if !modTime.IsZero() {
		at := unix.NsecToTimespec(modTime.UnixNano())
		if err := unix.UtimesNanoAt(dirfd, temp, []unix.Timespec{at, at}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("date %s temp: %w", label, err)
		}
	}
	if err := wakeRenameAt(dirfd, temp, dirfd, name); err != nil {
		return fmt.Errorf("install %s: %w", label, err)
	}
	tempPresent = false
	return nil
}

// writeWakeDialogEntryAt installs data as the entry name under the marker
// lock. Over a single-file marker or none it builds the directory, with a
// present marker as its legacy entry, and exchanges it in, then removes the
// file it replaced. Where the file system refuses the exchange, or the
// agent's wake reads only the single file (wakeReadsDialogEntries), the new
// dialog's marker replaces the single file as a single-file marker
// (writeWakeDialogSingleMarkerAt). A present directory always takes the
// entry; it goes when its last entry closes, and the single file follows.
func writeWakeDialogEntryAt(dirfd int, agentDir *wakeAgentDir, name string, data []byte) error {
	entries := wakeReadsDialogEntries(agentDir)
	return withWakeDialogMarkerLock(dirfd, func() error {
		dir, ok, err := openWakeDialogEntriesAt(dirfd, agentDir.path)
		if err != nil {
			return err
		}
		if ok {
			defer func() { _ = dir.Close() }()
			return writeWakeDialogEntryFileAt(int(dir.Fd()), name, data, time.Time{})
		}
		legacy, info, exists, err := readWakeRepairMetadataAt(dirfd, wakeDialogFileName, "wake dialog marker",
			filepath.Join(agentDir.path, wakeDialogFileName), maxWakeMetadataFileBytes)
		if err != nil {
			return err
		}
		if !entries {
			return writeWakeDialogSingleMarkerAt(dirfd, legacy, data)
		}
		temp, err := wakeDialogTempName("dir")
		if err != nil {
			return err
		}
		if err := unix.Mkdirat(dirfd, temp, 0o700); err != nil {
			return fmt.Errorf("create wake dialog entries: %w", err)
		}
		installed := false
		defer func() {
			if !installed {
				_ = removeWakeDialogTempDirAt(dirfd, temp)
			}
		}()
		tempFD, err := unix.Openat(dirfd, temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open wake dialog entries: %w", err)
		}
		defer func() { _ = unix.Close(tempFD) }()
		if err := writeWakeDialogEntryFileAt(tempFD, name, data, time.Time{}); err != nil {
			return err
		}
		if !exists {
			if err := wakeRenameNoReplaceAt(dirfd, temp, dirfd, wakeDialogFileName); err != nil {
				return fmt.Errorf("install wake dialog entries: %w", err)
			}
			installed = true
			return nil
		}
		if err := writeWakeDialogEntryFileAt(tempFD, wakeDialogLegacyEntry, legacy, info.ModTime()); err != nil {
			return err
		}
		if err := wakeDialogExchangeAt(dirfd, temp, dirfd, wakeDialogFileName); err != nil {
			return writeWakeDialogSingleMarkerAt(dirfd, legacy, data)
		}
		installed = true
		// temp now names the single file the entries replaced.
		if err := wakeUnlinkAt(dirfd, temp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	})
}

// wakeReadsDialogEntries reports whether the agent's wake reads the
// directory of entries: false only for a wake lock that is not stale and
// records a release before WakeDialogEntriesSince. Every such release
// records its version, so a lock without a release version (a local build,
// or a lock being created) is a wake that reads entries, and keeps the
// guard of each dialog.
func wakeReadsDialogEntries(agentDir *wakeAgentDir) bool {
	inspection := inspectWakeLock(filepath.Dir(filepath.Dir(agentDir.path)), filepath.Base(agentDir.path))
	if !inspection.Exists || inspection.Status == wakeLockStale {
		return true
	}
	reads, known := WakeVersionReadsDialogEntries(inspection.Lock.ImageVersion)
	return reads || !known
}

// wakeDialogExchangeAt swaps two names in one step (wakeRenameExchangeAt).
var wakeDialogExchangeAt = wakeRenameExchangeAt

// writeWakeDialogSingleMarkerAt keeps the single-file form, under the
// caller's marker lock, as it was before entries: the new marker replaces
// the present one, except that a tool call's marker keeps a present marker
// without a tool_use_id, whose hold closes only on a turn or session event.
func writeWakeDialogSingleMarkerAt(dirfd int, current, data []byte) error {
	var present, next wakeDialogMarker
	if json.Unmarshal(data, &next) == nil && next.ToolUseID != "" &&
		json.Unmarshal(current, &present) == nil && present.ToolUseID == "" {
		return nil
	}
	return writeWakeDialogEntryFileAt(dirfd, wakeDialogFileName, data, time.Time{})
}

// removeWakeDialogTempDirAt removes a temp entries directory that was never
// installed.
func removeWakeDialogTempDirAt(dirfd int, temp string) error {
	fd, err := unix.Openat(dirfd, temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), temp)
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		_ = wakeUnlinkAt(fd, name, 0)
	}
	return wakeUnlinkAt(dirfd, temp, unix.AT_REMOVEDIR)
}

// wakeDialogEntriesClosedBy reports whether a Claude Code hook event proves
// closed the dialog of a Claude Code dialog hook's marker, keyed entry or
// single file:
//   - an explicit close with no input closes all;
//   - a subagent's SubagentStop closes every entry of that agent_id;
//   - otherwise only events of the entry's own session: a tool call's close
//     closes only its own tool_use_id, SessionEnd (a clear included) all,
//     a SessionStart of a new process (startup or resume) all, and a
//     main-thread Stop or prompt only the main-thread entries. A subagent
//     can show a dialog while the main thread takes a prompt, so a prompt
//     proves nothing about it. Other SessionStart sources close nothing.
func wakeDialogEntriesClosedBy(raw []byte, in wakeDialogHookInput) bool {
	if in.emptyInput {
		return true
	}
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil {
		return false
	}
	switch {
	case in.Event == "SubagentStop":
		return in.AgentID != "" && marker.AgentID == in.AgentID
	case marker.SessionID != in.SessionID:
		return false
	case in.ToolUseID != "":
		return marker.ToolUseID == in.ToolUseID
	case in.Event == "SessionEnd":
		return true
	case in.Event == "SessionStart":
		return in.Source == "startup" || in.Source == "resume"
	case in.Event == "Stop", in.Event == "UserPromptSubmit":
		// A prompt's hook runs before its turn, while no main-thread
		// dialog can be on screen, so it closes every main-thread entry.
		return in.AgentID == "" && marker.AgentID == ""
	}
	return false
}

// claudeDialogHookMarker reports a marker the Claude Code dialog hook's open
// wrote: no harness, and a PreToolUse or PermissionRequest event.
func claudeDialogHookMarker(raw []byte) bool {
	var marker wakeDialogMarker
	return json.Unmarshal(raw, &marker) == nil && marker.Harness == "" && wakeDialogHookVerb(marker.Event) == "open"
}

// maxWakeDialogHostHops bounds the walk from the hook receiver to the
// process that ran the hook: its sh -c starts amq, or execs into it.
const maxWakeDialogHostHops = 2

// wakeDialogAncestors reads this process's proven ancestors (launch.Ancestors).
var wakeDialogAncestors = launch.Ancestors

// wakeDialogHookHost is the process that ran the hook, named by pid (the
// $PPID of the hook's sh -c): the one of this process's first
// maxWakeDialogHostHops ancestors, each edge proven by its start token, that
// has that pid, or nil. The identity is the walk's, so a pid outside that
// chain records no host.
func wakeDialogHookHost(pid int) *launch.HolderIdentity {
	if pid <= 0 {
		return nil
	}
	for i, ancestor := range wakeDialogAncestors() {
		if i == maxWakeDialogHostHops {
			break
		}
		if ancestor.PID == pid {
			return &ancestor
		}
	}
	return nil
}

// wakeDialogHostGone reports a marker whose recorded host process is proven
// gone: not running, or its pid now runs another process. A marker without
// a host, or a host the wake cannot prove gone, holds as before.
func wakeDialogHostGone(raw []byte) (int, bool) {
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Host == nil {
		return 0, false
	}
	state, _ := launch.InspectHolder(*marker.Host)
	return marker.Host.PID, state == launch.LeaseStale
}
