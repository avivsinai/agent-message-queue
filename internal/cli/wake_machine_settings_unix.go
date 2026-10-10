//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// The machine wake settings: ~/.amq/wake.settings sets wake settings for
// every wake of this user on this machine, under each agent's .wake.settings.
// It has the agent file's format and trust rules. Its writers serialize on
// wakeMachineSettingsLockFileName; a running wake reads it on each settings
// tick and each inbox scan, resolving the path every time.
const (
	wakeMachineSettingsFileName     = "wake.settings"
	wakeMachineSettingsLockFileName = "wake.settings.lock"
	wakeMachineSettingsLabel        = "machine wake settings"
	wakeMachineSettingsDirLabel     = "AMQ home directory"
)

func machineWakeSettingsPath() (string, error) {
	dir, err := amqhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, wakeMachineSettingsFileName), nil
}

// readMachineWakeSettings reads ~/.amq/wake.settings. A missing HOME,
// ~/.amq or file is no machine layer: exists=false, err=nil, whatever the
// mode of ~/.amq. ~/.amq must be a real directory owned by the user and not
// group/world-writable; the file a regular 0600 file owned by the user,
// opened without following a symlink. A failed trust check or read is an
// error with exists=true; a write renamed in during the read is a
// wakeSnapshotReadChangedError. It returns the raw bytes; the caller
// decodes them.
func readMachineWakeSettings() ([]byte, bool, error) {
	dir, err := amqhome.Dir()
	if err != nil {
		return nil, false, nil
	}
	// No file is no machine layer, before ~/.amq is trusted: a ~/.amq that
	// other writers accept (group-writable under umask 0002) must not mark
	// every wake's machine layer refused for a file nobody made.
	if _, err := os.Lstat(filepath.Join(dir, wakeMachineSettingsFileName)); errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, false, nil
	}
	home, err := openMachineWakeHome(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, true, err
	}
	defer func() { _ = home.Close() }()
	var raw []byte
	var exists bool
	var readErr error
	if err := home.withFD(func(dirfd int) error {
		raw, exists, readErr = readMachineWakeSettingsAt(dirfd, home)
		return nil
	}); err != nil {
		return nil, true, err
	}
	return raw, exists, readErr
}

func readMachineWakeSettingsAt(dirfd int, home *wakeAgentDir) ([]byte, bool, error) {
	raw, _, exists, err := readWakeRepairMetadataAt(
		dirfd,
		wakeMachineSettingsFileName,
		wakeMachineSettingsLabel,
		filepath.Join(home.path, wakeMachineSettingsFileName),
		maxWakeMetadataFileBytes,
	)
	return raw, exists, err
}

// readMachineWakeSettingsSettled is readMachineWakeSettings read again
// while a concurrent wake config --machine write renames the file in: such
// a read is not a refused file. After the bounded retries the last result
// is returned as it is.
func readMachineWakeSettingsSettled() (raw []byte, exists bool, err error) {
	raw, exists, err = readMachineWakeSettings()
	var snapshotChanged *wakeSnapshotReadChangedError
	for retry := 0; retry < wakeSettingsReadRetries && errors.As(err, &snapshotChanged); retry++ {
		time.Sleep(wakeSettingsReadRetryDelay)
		raw, exists, err = readMachineWakeSettings()
	}
	return raw, exists, err
}

// observeMachineWakeSettings is a starting wake's one settled read of the
// machine file.
func observeMachineWakeSettings() wakeSettingsObservation {
	return observeWakeSettings(readMachineWakeSettingsSettled())
}

// updateMachineWakeSettings is the read-modify-write of the machine file,
// under its lock in ~/.amq, which it creates (mode 0700) when missing. edit
// changes the stored keys. A current file that cannot be read or decoded is
// an error and is left as it is. The result must be valid alone over the
// defaults, or it is a usage error. It returns the bytes now stored.
func updateMachineWakeSettings(edit func(*wakeSettingsDoc) error) ([]byte, error) {
	return withMachineWakeSettings(func(dirfd int, home *wakeAgentDir) (wakeSettingsDoc, error) {
		raw, exists, err := readMachineWakeSettingsAt(dirfd, home)
		if err != nil {
			return wakeSettingsDoc{}, err
		}
		var doc wakeSettingsDoc
		if exists {
			if doc, err = decodeWakeSettingsDoc(raw); err != nil {
				return wakeSettingsDoc{}, fmt.Errorf("%s: %w", filepath.Join(home.path, wakeMachineSettingsFileName), err)
			}
		}
		if err := edit(&doc); err != nil {
			return wakeSettingsDoc{}, err
		}
		return doc, nil
	})
}

// resetMachineWakeSettings replaces the machine file with exactly keys from
// values, whatever the current file holds. The result must be valid alone
// over the defaults, or it is a usage error. It returns the bytes stored.
func resetMachineWakeSettings(values wakeSettings, keys []string) ([]byte, error) {
	return withMachineWakeSettings(func(int, *wakeAgentDir) (wakeSettingsDoc, error) {
		var doc wakeSettingsDoc
		doc.set(values, keys)
		return doc, nil
	})
}

// openMachineWakeHome opens ~/.amq with the wake directory trust checks. A
// group/world-writable ~/.amq, which other AMQ writers accept, is refused
// with the command that fixes it.
func openMachineWakeHome(dir string) (*wakeAgentDir, error) {
	home, err := openWakeDirectory(dir, wakeMachineSettingsDirLabel)
	if err != nil {
		if info, statErr := os.Lstat(dir); statErr == nil && info.IsDir() && info.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf("%w; run chmod 700 %s", err, dir)
		}
		return nil, err
	}
	return home, nil
}

// withMachineWakeSettings stores the doc next returns, under the machine
// file lock, after validating it alone over the defaults. ~/.amq is trusted
// before the lock file is made in it and again under the lock.
func withMachineWakeSettings(next func(dirfd int, home *wakeAgentDir) (wakeSettingsDoc, error)) ([]byte, error) {
	if !lock.AdvisoryLockAvailable() {
		return nil, errors.New("refusing to change the machine wake settings without an advisory file lock")
	}
	dir, err := amqhome.EnsureDir()
	if err != nil {
		return nil, err
	}
	home, err := openMachineWakeHome(dir)
	if err != nil {
		return nil, err
	}
	_ = home.Close()
	var stored []byte
	err = lock.WithExclusiveFileLock(filepath.Join(dir, wakeMachineSettingsLockFileName), func() error {
		home, err := openMachineWakeHome(dir)
		if err != nil {
			return err
		}
		defer func() { _ = home.Close() }()
		return home.withFD(func(dirfd int) error {
			doc, err := next(dirfd, home)
			if err != nil {
				return err
			}
			if _, err := doc.effective(); err != nil {
				return UsageError("%v", err)
			}
			if stored, err = doc.encode(); err != nil {
				return err
			}
			return writeWakeRepairMetadataAt(
				dirfd,
				home,
				wakeMachineSettingsFileName,
				wakeMachineSettingsLabel,
				stored,
				maxWakeMetadataFileBytes,
			)
		})
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}
