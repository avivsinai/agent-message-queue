//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// A settings write waits this long for the lifecycle guard, covering
	// startup contention with readiness publication or repair.
	wakeSettingsGuardTimeout = 5 * time.Second
	// The running wake re-reads .wake.settings this often. One small read
	// every 2 s bounds the latency of a live change for a held cohort.
	wakeSettingsPollInterval = 2 * time.Second
	// A read that a concurrent rename changed is retried this often.
	wakeSettingsReadRetries    = 5
	wakeSettingsReadRetryDelay = 20 * time.Millisecond
)

// readWakeSettingsFileAt reads .wake.settings on the agent dirfd. The file
// must be a regular 0600 file owned by the user.
func readWakeSettingsFileAt(dirfd int, agentDir *wakeAgentDir) ([]byte, bool, error) {
	raw, _, exists, err := readWakeRepairMetadataAt(
		dirfd,
		wakeSettingsFileName,
		"wake settings",
		filepath.Join(agentDir.path, wakeSettingsFileName),
		maxWakeMetadataFileBytes,
	)
	return raw, exists, err
}

// readWakeSettingsFile reads .wake.settings without the lifecycle guard.
// Writers install it by atomic rename, so a reader sees one whole version or
// a wakeSnapshotReadChangedError when a rename lands during the read.
func readWakeSettingsFile(agentDir *wakeAgentDir) ([]byte, bool, error) {
	var raw []byte
	var exists bool
	var readErr error
	if err := agentDir.withFD(func(dirfd int) error {
		raw, exists, readErr = readWakeSettingsFileAt(dirfd, agentDir)
		return nil
	}); err != nil {
		return nil, false, err
	}
	return raw, exists, readErr
}

func writeWakeSettingsFileAt(dirfd int, agentDir *wakeAgentDir, raw []byte) error {
	return writeWakeRepairMetadataAt(
		dirfd,
		agentDir,
		wakeSettingsFileName,
		"wake settings",
		raw,
		maxWakeMetadataFileBytes,
	)
}

// updateWakeSettingsFileInDir is the read-modify-write of .wake.settings,
// under the lifecycle guard. edit changes the stored keys; the whole result
// is validated before anything is written. A refused current file is an
// error and is left as it is. An invalid result is a usage error. It returns
// the bytes now stored and the effective settings.
func updateWakeSettingsFileInDir(
	agentDir *wakeAgentDir,
	edit func(*wakeSettingsDoc) error,
) ([]byte, wakeSettings, error) {
	path := filepath.Join(agentDir.path, wakeSettingsFileName)
	var stored []byte
	var settings wakeSettings
	err := withWakeLifecycleGuardModeAndTimeoutInDir(
		agentDir,
		unix.LOCK_EX|unix.LOCK_NB,
		wakeSettingsGuardTimeout,
		func(dirfd int) error {
			raw, exists, err := readWakeSettingsFileAt(dirfd, agentDir)
			if err != nil {
				return err
			}
			var doc wakeSettingsDoc
			if exists {
				if doc, err = decodeWakeSettingsDoc(raw); err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
			}
			if err := edit(&doc); err != nil {
				return err
			}
			if settings, err = doc.effective(); err != nil {
				return UsageError("%v", err)
			}
			if stored, err = doc.encode(); err != nil {
				return err
			}
			return writeWakeSettingsFileAt(dirfd, agentDir, stored)
		},
	)
	if err != nil {
		return nil, wakeSettings{}, err
	}
	return stored, settings, nil
}

// checkWakeSettingsForResume is the resume preflight's read-only check. A
// refused file refuses the upgrade while the old image still runs, instead
// of a resumed image that starts on defaults.
func checkWakeSettingsForResume(root, me string) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	raw, exists, readErr := readWakeSettingsFile(agentDir)
	// A concurrent wake config write is not a refused file; read it again.
	var snapshotChanged *wakeSnapshotReadChangedError
	for retry := 0; retry < wakeSettingsReadRetries && errors.As(readErr, &snapshotChanged); retry++ {
		time.Sleep(wakeSettingsReadRetryDelay)
		raw, exists, readErr = readWakeSettingsFile(agentDir)
	}
	if _, err := resolveWakeSettingsFile(raw, exists, readErr); err != nil {
		return fmt.Errorf(
			"wake resume preflight: %s is refused: %w",
			filepath.Join(agentDir.path, wakeSettingsFileName),
			err,
		)
	}
	return nil
}

// loadWakeSettingsAtStartup resolves a starting wake's settings after it
// holds the wake lock. explicit lists the settings flags given on the
// command line. On a fresh start they are written to the file under the
// lifecycle guard, and a refused file or failed write refuses the start. A
// resume never exits on the file: when its seed fails it runs with the flags
// in memory.
func loadWakeSettingsAtStartup(
	agentDir *wakeAgentDir,
	flags wakeSettings,
	explicit []string,
	resume bool,
) (wakeSettingsStartup, error) {
	if len(explicit) == 0 {
		raw, exists, readErr := readWakeSettingsFile(agentDir)
		return planWakeSettingsStartup(raw, exists, readErr, flags, explicit, resume)
	}
	var plan wakeSettingsStartup
	err := withWakeLifecycleGuardModeAndTimeoutInDir(
		agentDir,
		unix.LOCK_EX|unix.LOCK_NB,
		wakeSettingsGuardTimeout,
		func(dirfd int) error {
			raw, exists, readErr := readWakeSettingsFileAt(dirfd, agentDir)
			var err error
			plan, err = planWakeSettingsStartup(raw, exists, readErr, flags, explicit, resume)
			if err != nil || plan.write == nil {
				return err
			}
			return writeWakeSettingsFileAt(dirfd, agentDir, plan.write)
		},
	)
	if err != nil && resume {
		return unseededWakeSettingsResume(agentDir, flags, explicit, err)
	}
	if err != nil {
		return wakeSettingsStartup{}, fmt.Errorf(
			"store wake settings flags in %s: %w",
			filepath.Join(agentDir.path, wakeSettingsFileName),
			err,
		)
	}
	return plan, nil
}

// unseededWakeSettingsResume resolves a resume whose guarded seed failed with
// seedErr. A file that now exists wins as on any resume. With no file the
// wake runs the argv settings in memory and observes the absent file, so the
// next reload keeps them; the settings source retries the seed from the loop.
// No status is published for the absent file until the seed lands, so wake
// config does not report the argv settings as a refused or applied file.
func unseededWakeSettingsResume(
	agentDir *wakeAgentDir,
	flags wakeSettings,
	explicit []string,
	seedErr error,
) (wakeSettingsStartup, error) {
	raw, exists, readErr := readWakeSettingsFile(agentDir)
	plan, err := planWakeSettingsStartup(raw, exists, readErr, flags, explicit, true)
	if err != nil {
		plan = wakeSettingsStartup{settings: flags}
	}
	if err == nil && plan.write == nil {
		return plan, nil
	}
	plan.unseeded = fmt.Errorf(
		"store wake settings flags in %s: %w",
		filepath.Join(agentDir.path, wakeSettingsFileName),
		seedErr,
	)
	plan.seed = plan.write
	plan.write = nil
	plan.observed = observeWakeSettings(raw, exists, readErr)
	plan.applied = wakeSettingsAppliedStatus{}
	return plan, nil
}

// wakeSettingsSourceInDir is the running wake's settings source. It reads
// only through the canonical agent directory; a detached directory is
// reported unavailable so the reload skips it. seed is the file an unseeded
// resume still has to store for the wake generation in inspection: while the
// file is absent each read retries the guarded seed without waiting long for
// the guard, and returns the stored bytes once it lands. A file that appears
// otherwise is read as it is; only one read without error ends the seeding.
func wakeSettingsSourceInDir(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	seed []byte,
) func() ([]byte, bool, error) {
	return func() ([]byte, bool, error) {
		if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
			return nil, false, &wakeSettingsSourceUnavailableError{err: err}
		}
		if seed == nil {
			return readWakeSettingsFile(agentDir)
		}
		raw, exists, readErr, seedErr := seedWakeSettingsFile(agentDir, inspection, seed)
		if seedErr != nil {
			raw, exists, readErr = readWakeSettingsFile(agentDir)
		}
		// Only a file read without error ends the seeding. A refused or
		// changed file keeps the argv settings, and the guarded seed does
		// not write over it; once it is removed the next read seeds again.
		if exists && readErr == nil {
			seed = nil
		}
		return raw, exists, readErr
	}
}

// seedWakeSettingsFile stores seed as .wake.settings when the file is still
// absent and the wake generation in inspection still holds the lock. It
// returns the file the guarded read found or the seed it stored; seedErr
// means the guard, the generation check, or the write failed.
func seedWakeSettingsFile(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	seed []byte,
) (raw []byte, exists bool, readErr, seedErr error) {
	seedErr = withWakeLifecycleGuardModeAndTimeoutInDir(
		agentDir,
		unix.LOCK_EX|unix.LOCK_NB,
		wakeLifecycleGuardRetryTimeout,
		func(dirfd int) error {
			current := inspectWakeLockAt(dirfd, agentDir, inspection.Root, inspection.Agent)
			if !current.Exists ||
				current.Lock.Generation == "" ||
				current.Lock.Generation != inspection.Lock.Generation ||
				current.Root != inspection.Root || current.Agent != inspection.Agent {
				return fmt.Errorf("wake changed before settings seed")
			}
			raw, exists, readErr = readWakeSettingsFileAt(dirfd, agentDir)
			if exists || readErr != nil {
				return nil
			}
			if err := writeWakeSettingsFileAt(dirfd, agentDir, seed); err != nil {
				return err
			}
			raw, exists = seed, true
			return nil
		},
	)
	return raw, exists, readErr, seedErr
}

// wakeSettingsAppliedFile is .wake.settings.applied: what the wake of one
// lock generation did with one observed settings digest. It is a diagnostic,
// not a receipt, and is removed with the self-upgrade diagnostic.
type wakeSettingsAppliedFile struct {
	Schema     int    `json:"schema"`
	Root       string `json:"root"`
	Agent      string `json:"agent"`
	Generation string `json:"generation"`
	Status     string `json:"status"`
	Digest     string `json:"digest"`
	Error      string `json:"error"`
}

func readWakeSettingsAppliedAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (wakeSettingsAppliedFile, bool) {
	raw, _, exists, err := readWakeRepairMetadataAt(
		dirfd,
		wakeSettingsAppliedFileName,
		"wake settings applied status",
		filepath.Join(agentDir.path, wakeSettingsAppliedFileName),
		maxWakeMetadataFileBytes,
	)
	if err != nil || !exists {
		return wakeSettingsAppliedFile{}, false
	}
	var applied wakeSettingsAppliedFile
	if json.Unmarshal(raw, &applied) != nil ||
		applied.Schema != wakeSettingsSchemaV1 ||
		applied.Root != inspection.Root || applied.Agent != inspection.Agent ||
		applied.Generation == "" || applied.Generation != inspection.Lock.Generation {
		return wakeSettingsAppliedFile{}, false
	}
	return applied, true
}

// recordWakeSettingsApplied publishes status for the wake generation in
// inspection. It refuses when the lock generation changed and skips an
// unchanged write. It waits at most guardTimeout for the guard: the loop
// passes a short one, startup the settings guard timeout.
func recordWakeSettingsApplied(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	status wakeSettingsAppliedStatus,
	guardTimeout time.Duration,
) error {
	if agentDir == nil || !inspection.Exists || inspection.Lock.Generation == "" {
		return nil
	}
	return withWakeLifecycleGuardModeAndTimeoutInDir(agentDir, unix.LOCK_EX|unix.LOCK_NB, guardTimeout, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, inspection.Root, inspection.Agent)
		if !current.Exists ||
			current.Lock.Generation != inspection.Lock.Generation ||
			current.Root != inspection.Root || current.Agent != inspection.Agent {
			return fmt.Errorf("wake changed before settings status publication")
		}
		applied := wakeSettingsAppliedFile{
			Schema:     wakeSettingsSchemaV1,
			Root:       inspection.Root,
			Agent:      inspection.Agent,
			Generation: inspection.Lock.Generation,
			Status:     status.status,
			Digest:     status.digest,
			Error:      status.err,
		}
		if previous, exists := readWakeSettingsAppliedAt(dirfd, agentDir, inspection); exists && previous == applied {
			return nil
		}
		raw, err := json.Marshal(applied)
		if err != nil {
			return err
		}
		return writeWakeRepairMetadataAt(
			dirfd,
			agentDir,
			wakeSettingsAppliedFileName,
			"wake settings applied status",
			append(raw, '\n'),
			maxWakeMetadataFileBytes,
		)
	})
}

func removeWakeSettingsAppliedAt(dirfd int) error {
	err := wakeUnlinkAt(dirfd, wakeSettingsAppliedFileName, 0)
	if err == unix.ENOENT {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove wake settings applied status: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake settings applied status removal: %w", err)
	}
	return nil
}

// Running-wake states for one settings file digest. They are stable tokens
// for JSON output; text output renders none as "no running wake".
const (
	wakeSettingsRunApplied = "applied"
	wakeSettingsRunPending = "pending"
	wakeSettingsRunRefused = "refused"
	wakeSettingsRunNone    = "none"
	// A live wake holds the lock but its generation published no sidecar:
	// its image predates live settings and never reads the file.
	wakeSettingsRunUnreported = "unreported"
)

// wakeSettingsRunState is what the running wake did with a settings file.
type wakeSettingsRunState struct {
	Status     string // one of the wakeSettingsRun* values
	Generation string // lock generation; empty when no wake runs
	Error      string // the wake's refusal text when Status is refused
}

// inspectWakeSettingsRunState reports whether the wake that holds the lock
// now has applied or refused the settings file whose digest is fileDigest
// (wakeSettingsDigest of the current bytes). The generation is read from the
// lock on each call; a sidecar with another digest is pending, and a valid
// lock whose generation has no sidecar is unreported.
func inspectWakeSettingsRunState(
	agentDir *wakeAgentDir,
	root, me, fileDigest string,
) (wakeSettingsRunState, error) {
	var state wakeSettingsRunState
	err := agentDir.withFD(func(dirfd int) error {
		inspection := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !inspection.Exists ||
			inspection.Status == wakeLockMissing ||
			inspection.Status == wakeLockStale ||
			inspection.Lock.Generation == "" {
			state = wakeSettingsRunState{Status: wakeSettingsRunNone}
			return nil
		}
		state = wakeSettingsRunState{
			Status:     wakeSettingsRunPending,
			Generation: inspection.Lock.Generation,
		}
		applied, exists := readWakeSettingsAppliedAt(dirfd, agentDir, inspection)
		if !exists {
			// A lock still being created, or one not verified, may yet
			// record; only a valid lock with no sidecar is unreported.
			if inspection.Status == wakeLockValid {
				state.Status = wakeSettingsRunUnreported
			}
			return nil
		}
		if applied.Digest != fileDigest {
			return nil
		}
		switch applied.Status {
		case wakeSettingsStatusApplied:
			state.Status = wakeSettingsRunApplied
		case wakeSettingsStatusRefused:
			state.Status = wakeSettingsRunRefused
			state.Error = applied.Error
		}
		return nil
	})
	return state, err
}
