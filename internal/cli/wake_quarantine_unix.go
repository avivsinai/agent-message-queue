//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const wakeQuarantineTimestampLayout = "20060102T150405.000000000Z"

const wakeMalformedLockQuarantineAge = 2 * time.Second

var wakeQuarantineNow = time.Now

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
}

var beforeWakeQuarantineCleanupRevalidation = func(wakeQuarantineCleanupCandidate) {}

func parseWakeQuarantineName(name string) (time.Time, bool) {
	for _, source := range []string{".wake.lock", wakeTargetFileName} {
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

func scanWakeQuarantine(root string) []wakeQuarantineEntry {
	agentsDir := filepath.Join(root, "agents")
	agents, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}
	var quarantined []wakeQuarantineEntry
	for _, agent := range agents {
		if !agent.IsDir() {
			continue
		}
		path := filepath.Join(agentsDir, agent.Name())
		agentDir, err := openWakeDirectory(path, "wake quarantine agent directory")
		if err != nil {
			continue
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
		if readErr != nil {
			continue
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
	return quarantined
}

func checkWakeQuarantine(root string, now time.Time) opsWakeQuarantine {
	entries := scanWakeQuarantine(root)
	result := opsWakeQuarantine{Count: len(entries)}
	if len(entries) == 0 {
		return result
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
	return result
}

func findWakeQuarantineOlderThan(root string, cutoff time.Time) ([]wakeQuarantineCleanupCandidate, error) {
	entries := scanWakeQuarantine(root)
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
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, nil
}

func removeWakeQuarantineCandidate(root string, candidate wakeQuarantineCleanupCandidate) error {
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
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
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
		if err := unix.Unlinkat(dirfd, candidate.Name, 0); err != nil {
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

func wakeQuarantineName(source string, now time.Time) (string, error) {
	if source != ".wake.lock" && source != wakeTargetFileName {
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
	quarantine, err := wakeQuarantineName(source, wakeQuarantineNow())
	if err != nil {
		return "", err
	}
	if err := renameWakeNoReplaceAt(dirfd, source, dirfd, quarantine); err != nil {
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
	if !wakeLockEligibleForQuarantine(current, wakeQuarantineNow()) {
		return "", fmt.Errorf("wake lock is not eligible for quarantine")
	}
	return moveWakeQuarantineAt(
		dirfd,
		agentDir,
		".wake.lock",
		"wake lock",
		wakeQuarantineSnapshot{Raw: current.raw, FileInfo: current.fileInfo},
	)
}
