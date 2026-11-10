//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const wakeQuarantineTimestampLayout = "20060102T150405.000000000Z"

const wakeMalformedLockQuarantineAge = 2 * time.Second

type wakeQuarantineSnapshot struct {
	Raw      []byte
	FileInfo os.FileInfo
}

type wakeQuarantineEntry struct {
	Agent       string
	Name        string
	Path        string
	Quarantined time.Time
}

type wakeQuarantineCleanupCandidate struct {
	wakeQuarantineEntry
	Snapshot wakeQuarantineSnapshot
	// ReloadSocket marks a unix socket a former wake build left for its
	// reload transport; Snapshot.FileInfo is its identity and Raw is empty.
	ReloadSocket bool
}

// wakeReloadSocketPrefix names the sockets (and their staging names) that
// former wake builds created in the agent directory for the reload transport.
const wakeReloadSocketPrefix = ".wr."

var beforeWakeQuarantineCleanupRevalidation = func(wakeQuarantineCleanupCandidate) {}

func parseWakeQuarantineName(name string) (time.Time, bool) {
	for _, source := range []string{wakeLockFileName, wakeTargetFileName, wakeRestartFileName} {
		prefix := source + ".quarantined."
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rawTimestamp := strings.TrimPrefix(name, prefix)
		quarantined, err := time.Parse(wakeQuarantineTimestampLayout, rawTimestamp)
		if err != nil || quarantined.Format(wakeQuarantineTimestampLayout) != rawTimestamp {
			return time.Time{}, false
		}
		return quarantined, true
	}
	return time.Time{}, false
}

// readWakeQuarantineAgentEntries lists one agent directory through a
// validated directory capability.
func readWakeQuarantineAgentEntries(path string) ([]os.DirEntry, error) {
	agentDir, err := openWakeDirectory(path, "wake quarantine agent directory")
	if err != nil {
		return nil, err
	}
	var entries []os.DirEntry
	readErr := agentDir.withFD(func(dirfd int) error {
		duplicate, err := unix.Dup(dirfd)
		if err != nil {
			return err
		}
		dir := os.NewFile(uintptr(duplicate), path)
		defer func() { _ = dir.Close() }()
		entries, err = dir.ReadDir(-1)
		return err
	})
	_ = agentDir.Close()
	return entries, readErr
}

func scanWakeQuarantine(root string) ([]wakeQuarantineEntry, error) {
	agentsDir := filepath.Join(root, "agents")
	agents, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan wake quarantine agents directory: %w", err)
	}
	var quarantined []wakeQuarantineEntry
	for _, agent := range agents {
		if !agent.IsDir() {
			continue
		}
		path := filepath.Join(agentsDir, agent.Name())
		entries, err := readWakeQuarantineAgentEntries(path)
		if err != nil {
			return nil, fmt.Errorf("scan wake quarantine for agent %s: %w", agent.Name(), err)
		}
		for _, entry := range entries {
			when, ok := parseWakeQuarantineName(entry.Name())
			if !ok {
				continue
			}
			quarantined = append(quarantined, wakeQuarantineEntry{
				Agent:       agent.Name(),
				Name:        entry.Name(),
				Path:        filepath.Join(path, entry.Name()),
				Quarantined: when,
			})
		}
	}
	return quarantined, nil
}

func checkWakeQuarantine(root string, now time.Time) (opsWakeQuarantine, error) {
	entries, err := scanWakeQuarantine(root)
	if err != nil {
		return opsWakeQuarantine{}, err
	}
	result := opsWakeQuarantine{Count: len(entries)}
	if len(entries) == 0 {
		return result, nil
	}
	newest := entries[0].Quarantined
	for _, entry := range entries[1:] {
		if entry.Quarantined.After(newest) {
			newest = entry.Quarantined
		}
	}
	age := now.Sub(newest).Seconds()
	if age < 0 {
		age = 0
	}
	age = float64(int64(age + 0.5))
	result.NewestAgeSeconds = &age
	return result, nil
}

func findWakeQuarantineOlderThan(root string, cutoff time.Time) ([]wakeQuarantineCleanupCandidate, error) {
	entries, err := scanWakeQuarantine(root)
	if err != nil {
		return nil, err
	}
	candidates := make([]wakeQuarantineCleanupCandidate, 0, len(entries))
	for _, entry := range entries {
		if !entry.Quarantined.Before(cutoff) {
			continue
		}
		agentPath := filepath.Join(root, "agents", entry.Agent)
		agentDir, err := openWakeDirectory(agentPath, "wake quarantine agent directory")
		if err != nil {
			return nil, err
		}
		var snapshot wakeQuarantineSnapshot
		var exists bool
		readErr := agentDir.withFD(func(dirfd int) error {
			var err error
			snapshot, exists, err = readWakeQuarantineSnapshotAt(
				dirfd,
				agentDir,
				entry.Name,
				"wake quarantine artifact",
			)
			return err
		})
		_ = agentDir.Close()
		if readErr != nil {
			return nil, readErr
		}
		if !exists {
			continue
		}
		candidates = append(candidates, wakeQuarantineCleanupCandidate{
			wakeQuarantineEntry: entry,
			Snapshot:            snapshot,
		})
	}
	sockets, err := findWakeReloadSocketsOlderThan(root, cutoff)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, sockets...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, nil
}

// findWakeReloadSocketsOlderThan lists the reload sockets of stopped wakes
// whose last change is before cutoff. It never follows a symlink and takes
// only socket-type files named with the exact prefix.
func findWakeReloadSocketsOlderThan(root string, cutoff time.Time) ([]wakeQuarantineCleanupCandidate, error) {
	agentsDir := filepath.Join(root, "agents")
	agents, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan wake reload sockets agents directory: %w", err)
	}
	var candidates []wakeQuarantineCleanupCandidate
	for _, agent := range agents {
		if !agent.IsDir() {
			continue
		}
		path := filepath.Join(agentsDir, agent.Name())
		entries, err := readWakeQuarantineAgentEntries(path)
		if err != nil {
			return nil, fmt.Errorf("scan wake reload sockets for agent %s: %w", agent.Name(), err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), wakeReloadSocketPrefix) || entry.Type()&os.ModeSocket == 0 {
				continue
			}
			socketPath := filepath.Join(path, entry.Name())
			info, err := os.Lstat(socketPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("stat wake reload socket: %w", err)
			}
			if info.Mode()&os.ModeSocket == 0 || !info.ModTime().Before(cutoff) {
				continue
			}
			candidates = append(candidates, wakeQuarantineCleanupCandidate{
				wakeQuarantineEntry: wakeQuarantineEntry{
					Agent:       agent.Name(),
					Name:        entry.Name(),
					Path:        socketPath,
					Quarantined: info.ModTime(),
				},
				Snapshot:     wakeQuarantineSnapshot{FileInfo: info},
				ReloadSocket: true,
			})
		}
	}
	return candidates, nil
}

// removeWakeReloadSocketAt unlinks the candidate socket under the agent's
// lifecycle guard only while it is the same socket and no live or uncertain
// wake owns the directory.
func removeWakeReloadSocketAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	candidate wakeQuarantineCleanupCandidate,
) error {
	if candidate.Snapshot.FileInfo == nil || !strings.HasPrefix(candidate.Name, wakeReloadSocketPrefix) ||
		strings.ContainsRune(candidate.Name, '/') {
		return fmt.Errorf("wake reload socket cleanup candidate is invalid")
	}
	if lock := inspectWakeLockAt(dirfd, agentDir, root, candidate.Agent); lock.Exists && lock.Status != wakeLockStale {
		return nil
	}
	current, err := os.Lstat(filepath.Join(agentDir.path, candidate.Name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat wake reload socket: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(dirfd, candidate.Name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat wake reload socket: %w", err)
	}
	if current.Mode()&os.ModeSocket == 0 || stat.Mode&unix.S_IFMT != unix.S_IFSOCK ||
		!sameWakeFileIdentity(candidate.Snapshot.FileInfo, current) {
		return newWakeSnapshotReadChangedError(
			fmt.Errorf("wake reload socket changed before cleanup; preserving it"),
		)
	}
	if err := wakeUnlinkAt(dirfd, candidate.Name, 0); err != nil {
		return fmt.Errorf("remove exact wake reload socket: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake reload socket cleanup: %w", err)
	}
	return nil
}

func removeWakeQuarantineCandidate(root string, candidate wakeQuarantineCleanupCandidate) error {
	if candidate.ReloadSocket {
		agentDir, err := openWakeDirectory(
			filepath.Join(root, "agents", candidate.Agent),
			"wake quarantine agent directory",
		)
		if err != nil {
			return err
		}
		defer func() { _ = agentDir.Close() }()
		return withWakeLifecycleGuardInDir(agentDir, wakeGuardWait, func(dirfd int) error {
			return removeWakeReloadSocketAt(dirfd, agentDir, root, candidate)
		})
	}
	if err := assertNotWakeLockName(candidate.Name); err != nil {
		return err
	}
	when, exact := parseWakeQuarantineName(candidate.Name)
	if !exact || !when.Equal(candidate.Quarantined) {
		return fmt.Errorf("wake quarantine cleanup candidate name changed")
	}
	agentPath := filepath.Join(root, "agents", candidate.Agent)
	agentDir, err := openWakeDirectory(agentPath, "wake quarantine agent directory")
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return withWakeLifecycleGuardInDir(agentDir, wakeGuardWait, func(dirfd int) error {
		beforeWakeQuarantineCleanupRevalidation(candidate)
		current, exists, err := readWakeQuarantineSnapshotAt(
			dirfd,
			agentDir,
			candidate.Name,
			"wake quarantine artifact",
		)
		if err != nil {
			return err
		}
		if !exists || candidate.Snapshot.FileInfo == nil || current.FileInfo == nil ||
			!sameWakeFileIdentity(candidate.Snapshot.FileInfo, current.FileInfo) ||
			!bytes.Equal(candidate.Snapshot.Raw, current.Raw) {
			return newWakeSnapshotReadChangedError(
				fmt.Errorf("wake quarantine artifact changed before cleanup; preserving it"),
			)
		}
		if err := wakeUnlinkAt(dirfd, candidate.Name, 0); err != nil {
			return fmt.Errorf("remove exact wake quarantine artifact: %w", err)
		}
		if err := syncWakeOwnerDirFD(dirfd); err != nil {
			return fmt.Errorf("sync wake quarantine cleanup: %w", err)
		}
		return nil
	})
}

func wakeLockEligibleForQuarantine(inspection wakeLockInspection, now time.Time) bool {
	if !inspection.Exists || inspection.Status != wakeLockUnverified ||
		inspection.fileInfo == nil || !inspection.fileInfo.Mode().IsRegular() ||
		inspection.fileInfo.Mode().Perm() != 0o600 || inspection.observationErr != nil ||
		json.Valid(inspection.raw) || now.Sub(inspection.fileInfo.ModTime()) < wakeMalformedLockQuarantineAge {
		return false
	}
	return !malformedWakeLockLooksOwnerShaped(inspection.raw)
}

func malformedWakeLockLooksOwnerShaped(raw []byte) bool {
	for _, value := range raw {
		// Escaped or non-ASCII field names cannot be classified safely from an
		// invalid JSON envelope. Preserve them rather than guessing generic.
		if value == '\\' || value >= 0x80 {
			return true
		}
	}
	return bytes.Contains(bytes.ToLower(raw), []byte("owner"))
}

func malformedWakeTargetLooksOwnerShaped(raw []byte) bool {
	for searchAt := 0; searchAt < len(raw); {
		relative := bytes.IndexByte(raw[searchAt:], '"')
		if relative < 0 {
			return malformedWakeTargetHasBareOwnerMarker(raw)
		}
		keyStart := searchAt + relative
		keyEnd := keyStart + 1
		for keyEnd < len(raw) {
			if raw[keyEnd] == '\\' {
				keyEnd += 2
				continue
			}
			if raw[keyEnd] == '"' {
				break
			}
			keyEnd++
		}
		if keyEnd >= len(raw) {
			key, err := strconv.Unquote(string(raw[keyStart:]) + `"`)
			if err != nil {
				return true
			}
			return strings.EqualFold(key, "owner")
		}
		key, err := strconv.Unquote(string(raw[keyStart : keyEnd+1]))
		if err != nil {
			return true
		}
		valueAt := keyEnd + 1
		for valueAt < len(raw) && isJSONWhitespace(raw[valueAt]) {
			valueAt++
		}
		if valueAt >= len(raw) || raw[valueAt] != ':' {
			if strings.EqualFold(key, "owner") {
				return true
			}
			searchAt = keyEnd + 1
			continue
		}
		if !strings.EqualFold(key, "owner") {
			searchAt = keyEnd + 1
			continue
		}
		valueAt++
		for valueAt < len(raw) && isJSONWhitespace(raw[valueAt]) {
			valueAt++
		}
		if !bytes.HasPrefix(raw[valueAt:], []byte("null")) {
			return true
		}
		afterNull := valueAt + len("null")
		for afterNull < len(raw) && isJSONWhitespace(raw[afterNull]) {
			afterNull++
		}
		if afterNull < len(raw) && raw[afterNull] != ',' && raw[afterNull] != '}' {
			return true
		}
		searchAt = afterNull
	}
	return malformedWakeTargetHasBareOwnerMarker(raw)
}

func malformedWakeTargetHasBareOwnerMarker(raw []byte) bool {
	lower := bytes.ToLower(raw)
	marker := []byte("owner")
	for searchAt := 0; searchAt < len(lower); {
		relative := bytes.Index(lower[searchAt:], marker)
		if relative < 0 {
			return false
		}
		start := searchAt + relative
		end := start + len(marker)
		if start > 0 && isMalformedWakeTargetIdentifierByte(lower[start-1]) {
			searchAt = end
			continue
		}
		if end < len(lower) && isMalformedWakeTargetIdentifierByte(lower[end]) {
			searchAt = end
			continue
		}
		for end < len(lower) && isJSONWhitespace(lower[end]) {
			end++
		}
		if end == len(lower) || lower[end] == ':' {
			return true
		}
		searchAt = end + 1
	}
	return false
}

func isMalformedWakeTargetIdentifierByte(value byte) bool {
	return value == '_' || value == '$' ||
		(value >= 'a' && value <= 'z') ||
		(value >= '0' && value <= '9')
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func wakeQuarantineName(source string, now time.Time) (string, error) {
	if source != wakeLockFileName && source != wakeTargetFileName && source != wakeRestartFileName {
		return "", fmt.Errorf("unsupported wake quarantine source %q", source)
	}
	return source + ".quarantined." + now.UTC().Format(wakeQuarantineTimestampLayout), nil
}

func readWakeQuarantineSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
	label string,
) (wakeQuarantineSnapshot, bool, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return wakeQuarantineSnapshot{}, false, fmt.Errorf("invalid %s name %q", label, name)
	}
	path := filepath.Join(agentDir.path, name)
	open := func() (*os.File, error) {
		fd, err := unix.Openat(
			dirfd,
			name,
			unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		if err == unix.ENOENT {
			return wakeQuarantineSnapshot{}, false, nil
		}
		return wakeQuarantineSnapshot{}, true, fmt.Errorf("open %s: %w", label, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return wakeQuarantineSnapshot{}, true, fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return wakeQuarantineSnapshot{}, true, fmt.Errorf("%s must be a regular file", label)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return wakeQuarantineSnapshot{}, true, fmt.Errorf("%s mode is %o, want 0600", label, got)
	}
	if err := validateWakeTargetPathOwnership(label, path, info); err != nil {
		return wakeQuarantineSnapshot{}, true, err
	}
	raw, err := readWakeMetadata(file, label, path)
	if err != nil {
		return wakeQuarantineSnapshot{}, true, err
	}
	pathFile, err := open()
	if err != nil {
		return wakeQuarantineSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("%s changed while reopening: %w", label, err),
		)
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return wakeQuarantineSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("%s changed while restating: %w", label, statErr),
		)
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		return wakeQuarantineSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("%s changed while opening", label),
		)
	}
	return wakeQuarantineSnapshot{Raw: raw, FileInfo: info}, true, nil
}

func moveWakeQuarantineAt(
	dirfd int,
	agentDir *wakeAgentDir,
	source string,
	label string,
	expected wakeQuarantineSnapshot,
) (string, error) {
	quarantine, err := wakeQuarantineName(source, time.Now())
	if err != nil {
		return "", err
	}
	if err := wakeRenameNoReplaceAt(dirfd, source, dirfd, quarantine); err != nil {
		return "", fmt.Errorf("quarantine exact %s as %s: %w", label, quarantine, err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return "", fmt.Errorf("sync %s quarantine %s: %w", label, quarantine, err)
	}
	moved, exists, err := readWakeQuarantineSnapshotAt(
		dirfd,
		agentDir,
		quarantine,
		label+" quarantine",
	)
	if err != nil {
		return "", err
	}
	if !exists || expected.FileInfo == nil || moved.FileInfo == nil ||
		!os.SameFile(expected.FileInfo, moved.FileInfo) ||
		!bytes.Equal(expected.Raw, moved.Raw) {
		return "", fmt.Errorf("%s quarantine %s failed exact inode/raw verification", label, quarantine)
	}
	return quarantine, nil
}

func quarantineWakeTargetAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeTargetSnapshot,
) (string, error) {
	current, exists, err := readWakeQuarantineSnapshotAt(
		dirfd,
		agentDir,
		wakeTargetFileName,
		"wake target",
	)
	if err != nil {
		return "", err
	}
	if !exists || expected.FileInfo == nil || current.FileInfo == nil ||
		!sameWakeFileIdentity(expected.FileInfo, current.FileInfo) ||
		!bytes.Equal(expected.Raw, current.Raw) {
		return "", newWakeSnapshotReadChangedError(
			fmt.Errorf("wake target changed before quarantine"),
		)
	}
	return moveWakeQuarantineAt(
		dirfd,
		agentDir,
		wakeTargetFileName,
		"wake target",
		current,
	)
}

func quarantineWakeRestartRecordAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeRestartRecordSnapshot,
) (string, error) {
	current, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
	if err != nil && (!exists || current.Object.FileInfo == nil) {
		return "", err
	}
	if !exists || !sameWakeRestartObjectSnapshot(expected, current) {
		return "", newWakeSnapshotReadChangedError(
			fmt.Errorf("wake restart request changed before quarantine"),
		)
	}
	return moveWakeQuarantineAt(
		dirfd,
		agentDir,
		wakeRestartFileName,
		"wake restart request",
		current.Object,
	)
}

func quarantineMalformedWakeLockAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
) (string, error) {
	current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !sameWakeLockGeneration(expected, current) {
		return "", newWakeSnapshotReadChangedError(
			fmt.Errorf("wake lock changed before quarantine"),
		)
	}
	if !wakeLockEligibleForQuarantine(current, time.Now()) {
		return "", fmt.Errorf("wake lock is not eligible for quarantine")
	}
	return moveWakeQuarantineAt(
		dirfd,
		agentDir,
		wakeLockFileName,
		"wake lock",
		wakeQuarantineSnapshot{Raw: current.raw, FileInfo: current.fileInfo},
	)
}
