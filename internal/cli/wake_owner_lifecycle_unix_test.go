//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestValidateAuthoritativeWakeOwnerSeparatesStrictClaimsFromLegacyParsing(t *testing.T) {
	valid := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	if err := validateWakeOwner(valid); err != nil {
		t.Fatalf("legacy validator rejected valid owner: %v", err)
	}
	if err := validateAuthoritativeWakeOwner(valid); err != nil {
		t.Fatalf("strict validator rejected valid owner: %v", err)
	}

	tests := []struct {
		name  string
		owner wakeOwner
		want  string
	}{
		{name: "missing process start", owner: wakeOwner{PID: 4242, BootID: valid.BootID, SessionID: 99}, want: "process start"},
		{name: "missing boot id", owner: wakeOwner{PID: 4242, ProcessStart: valid.ProcessStart, SessionID: 99}, want: "boot id"},
		{name: "zero session", owner: wakeOwner{PID: 4242, ProcessStart: valid.ProcessStart, BootID: valid.BootID}, want: "session"},
		{name: "surrounding process whitespace", owner: wakeOwner{PID: 4242, ProcessStart: " 12345 ", BootID: valid.BootID, SessionID: 99}, want: "canonical"},
		{name: "surrounding boot whitespace", owner: wakeOwner{PID: 4242, ProcessStart: valid.ProcessStart, BootID: " " + valid.BootID + " ", SessionID: 99}, want: "canonical"},
		{name: "process control character", owner: wakeOwner{PID: 4242, ProcessStart: "123\n45", BootID: valid.BootID, SessionID: 99}, want: "control"},
		{name: "boot control character", owner: wakeOwner{PID: 4242, ProcessStart: valid.ProcessStart, BootID: "11111111-1111-1111-\t1111-111111111111", SessionID: 99}, want: "control"},
		{name: "malformed process start", owner: wakeOwner{PID: 4242, ProcessStart: "owner-start", BootID: valid.BootID, SessionID: 99}, want: "malformed platform"},
		{name: "noncanonical process start", owner: wakeOwner{PID: 4242, ProcessStart: "012345", BootID: valid.BootID, SessionID: 99}, want: "malformed platform"},
		{name: "malformed boot id", owner: wakeOwner{PID: 4242, ProcessStart: valid.ProcessStart, BootID: "boot-1", SessionID: 99}, want: "malformed platform"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateWakeOwner(test.owner); err != nil {
				t.Fatalf("legacy validator must keep record readable: %v", err)
			}
			err := validateAuthoritativeWakeOwner(test.owner)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("strict validator error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestOwnerWakeLockFenceRequiresStrictSchemaAndMode(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-fence-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	lock := bindWakeLockToTarget(wakeLock{
		PID:          5151,
		TTY:          "unknown",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--me", "codex", "--root", root},
		Generation:   "owner-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != lock.PID {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: lock.ProcessStart,
			BootID:     lock.BootID,
			Executable: lock.Executable,
			Args:       lock.Args,
		}
	})

	write := func(t *testing.T, value wakeLock, mode os.FileMode) {
		t.Helper()
		lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		path := writeWakeLockForTest(t, root, "codex", value)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("strict 0400 owner schema is readable", func(t *testing.T) {
		write(t, lock, wakeOwnerLockFileMode)
		inspection := inspectWakeLock(root, "codex")
		if inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
			t.Fatalf("owner lock inspection = %#v, want confirmed valid", inspection)
		}
		if !sameWakeOwner(inspection.Lock.Owner, &owner) {
			t.Fatalf("owner lock owner = %#v, want %#v", inspection.Lock.Owner, owner)
		}
	})

	t.Run("0400 without owner schema is unverified", func(t *testing.T) {
		malformed := lock
		malformed.OwnerSchema = 0
		write(t, malformed, wakeOwnerLockFileMode)
		inspection := inspectWakeLock(root, "codex")
		if inspection.Status != wakeLockUnverified ||
			!strings.Contains(inspection.Reason, "owner schema") ||
			!strings.Contains(inspection.Reason, "newer amq") {
			t.Fatalf("malformed owner lock inspection = %#v, want newer-amq owner-schema refusal", inspection)
		}
	})

	t.Run("0400 requires strict wake process identity", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			mutate func(*wakeLock)
			want   string
		}{
			{
				name:   "missing process start",
				mutate: func(value *wakeLock) { value.ProcessStart = "" },
				want:   "wake process start is required",
			},
			{
				name:   "missing boot id",
				mutate: func(value *wakeLock) { value.BootID = "" },
				want:   "wake boot id is required",
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				malformed := lock
				test.mutate(&malformed)
				write(t, malformed, wakeOwnerLockFileMode)
				inspection := inspectWakeLock(root, "codex")
				if inspection.Status != wakeLockUnverified || !strings.Contains(inspection.Reason, test.want) {
					t.Fatalf("owner wake identity inspection = %#v, want %q", inspection, test.want)
				}
				reused := wakeProcessInfo{
					PID:        malformed.PID,
					Running:    true,
					StartToken: "99999",
					BootID:     owner.BootID,
					Executable: "/usr/local/bin/amq",
					Args:       malformed.Args,
				}
				state, _ := classifyWakeIdentity(inspection, reused)
				if state != wakeIdentityUnknown {
					t.Fatalf("malformed owner wake classified %s against matching argv, want unknown", state)
				}
			})
		}
	})

	t.Run("malformed 0400 record surfaces newer amq through acquisition", func(t *testing.T) {
		lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(lockPath, []byte("{not-json"), wakeOwnerLockFileMode); err != nil {
			t.Fatal(err)
		}
		inspection := inspectWakeLock(root, "codex")
		if inspection.Status != wakeLockUnverified || !strings.Contains(inspection.Reason, "newer amq") {
			t.Fatalf("malformed 0400 inspection = %#v, want newer-amq refusal", inspection)
		}
		if _, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{}); err == nil ||
			!strings.Contains(err.Error(), "newer amq") {
			t.Fatalf("generic acquisition error = %v, want newer-amq diagnostic", err)
		}
		requested := target
		if _, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target:   &requested,
			wakeMode: wakeTargetInjectVia,
		}); err == nil || !strings.Contains(err.Error(), "newer amq") {
			t.Fatalf("owner acquisition error = %v, want newer-amq diagnostic", err)
		}
	})

	t.Run("0600 owner markers are unverified corruption", func(t *testing.T) {
		write(t, lock, 0o600)
		inspection := inspectWakeLock(root, "codex")
		if inspection.Status != wakeLockUnverified || !strings.Contains(inspection.Reason, "owner") {
			t.Fatalf("legacy-mode owner lock inspection = %#v, want owner-marker refusal", inspection)
		}
	})
}

func TestAuthoritativeOwnerLockRequiresTheSameStrictOwnerInTarget(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "owner-pair-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	lock := wakeLock{
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		WakeMode:     wakeOwnerWakeMode,
		TargetDigest: mustWakeTargetDigest(target),
		Generation:   "owner-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}
	if err := validateWakeTargetMatchesLock(lock, target); err != nil {
		t.Fatalf("strict owner pair rejected: %v", err)
	}

	missing := target
	missing.Owner = nil
	missingLock := lock
	missingLock.TargetDigest = mustWakeTargetDigest(missing)
	if err := validateWakeTargetMatchesLock(missingLock, missing); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("missing target owner error = %v, want owner refusal", err)
	}

	different := target
	other := owner
	other.PID++
	different.Owner = &other
	differentLock := lock
	differentLock.TargetDigest = mustWakeTargetDigest(different)
	if err := validateWakeTargetMatchesLock(differentLock, different); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("different target owner error = %v, want owner refusal", err)
	}
}

func TestOwnerAcquisitionNeverDegradesUnsupportedOrUnknownObservation(t *testing.T) {
	tests := []struct {
		name        string
		observation wakeOwnerObservation
		observeErr  error
		want        string
	}{
		{
			name: "unsupported observer",
			observation: wakeOwnerObservation{
				State:                 wakeOwnerUnknown,
				Reason:                "kernel owner observation unsupported",
				CapabilityUnsupported: true,
			},
			observeErr: errors.New("owner observation unsupported"),
			want:       "owner observation unsupported",
		},
		{
			name:        "unknown observer",
			observation: wakeOwnerObservation{State: wakeOwnerUnknown, Reason: "identity incomplete"},
			want:        "requested wake owner is unknown",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			owner := wakeOwner{
				PID:          4242,
				ProcessStart: "12345",
				BootID:       "11111111-1111-1111-1111-111111111111",
				SessionID:    99,
			}
			injector := writeExecutableForTest(t, "owner-observation-refusal-injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
			target.Owner = &owner
			oldObserve := observeAuthoritativeWakeOwner
			observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
				return tc.observation, tc.observeErr
			}
			t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

			_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target:   &target,
				wakeMode: wakeTargetInjectVia,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("owner acquisition error = %v, want %q", err, tc.want)
			}
			if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
				t.Fatalf("refused owner acquisition published wake lock: %#v", inspection)
			}
			if _, exists, err := readWakeTarget(root, "codex"); err != nil || exists {
				t.Fatalf("refused owner acquisition target exists=%v err=%v", exists, err)
			}
			if !sameWakeOwner(target.Owner, &owner) {
				t.Fatalf("refused owner acquisition mutated requested owner: %#v", target.Owner)
			}
		})
	}
}

func TestPublishAuthoritativeWakeClaimCommitsACompleteReadOnlyGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-publish-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	lock := bindWakeLockToTarget(wakeLock{
		PID:          5151,
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		Started:      "2026-07-23T00:00:00Z",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--me", "codex", "--root", root},
		Generation:   "owner-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, "codex", target, lock)
	}); err != nil {
		t.Fatalf("publish owner claim: %v", err)
	}

	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	targetPath := wakeTargetPath(root, "codex")
	lockInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := lockInfo.Mode().Perm(); got != wakeOwnerLockFileMode {
		t.Fatalf("owner lock mode = %o, want %o", got, wakeOwnerLockFileMode)
	}
	targetInfo, err := os.Lstat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("owner target mode = %o, want 0600", got)
	}
	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted wakeLock
	if err := json.Unmarshal(lockData, &persisted); err != nil {
		t.Fatalf("parse committed owner lock: %v", err)
	}
	if persisted.OwnerSchema != wakeOwnerLockSchema || persisted.WakeMode != wakeOwnerWakeMode ||
		!sameWakeOwner(persisted.Owner, &owner) || persisted.TargetDigest != mustWakeTargetDigest(target) {
		t.Fatalf("committed owner lock = %#v", persisted)
	}

	replacement := lock
	replacement.Generation = "replacement-generation"
	replacementTarget := target
	replacementTarget.Created = "2026-07-23T01:00:00Z"
	replacement.TargetDigest = mustWakeTargetDigest(replacementTarget)
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, "codex", replacementTarget, replacement)
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("replacement publication error = %v, want no-replace refusal", err)
	}
	afterInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	afterData, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !sameWakeFileIdentity(lockInfo, afterInfo) || string(afterData) != string(lockData) {
		t.Fatal("existing owner lock changed after no-replace refusal")
	}
}

func TestPublishAuthoritativeWakeClaimDirectorySyncFailureIsNotDegradable(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-sync-failure-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner
	lock := bindWakeLockToTarget(wakeLock{
		PID:          5151,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Generation:   "owner-sync-failure-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode

	originalSync := syncWakeOwnerDirFD
	syncWakeOwnerDirFD = func(int) error { return syscall.ENOSYS }
	t.Cleanup(func() { syncWakeOwnerDirFD = originalSync })

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, "codex", target, lock)
	})
	var publicationErr *wakeOwnerPublicationError
	if !errors.As(err, &publicationErr) {
		t.Fatalf("publication error = %v, want typed durability failure", err)
	}
	if publicationErr.Unsupported {
		t.Fatalf("directory-sync failure was marked degradable: %#v", publicationErr)
	}
	if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
		t.Fatalf("directory-sync failure published a lock: %#v", inspection)
	}
}

func TestReleasedWakeTargetCleanupPreservesAReplacementSnapshot(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-release-snapshot-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		snapshot, exists, err := readWakeTargetSnapshotAt(dirfd, agentDir, root, "codex")
		if err != nil || !exists {
			return errors.New("initial wake target snapshot is unavailable")
		}
		replacement := target
		replacement.Created = "2026-07-23T02:00:00Z"
		data, err := json.MarshalIndent(replacement, "", "  ")
		if err != nil {
			return err
		}
		replacementPath := wakeTargetPath(root, "codex") + ".replacement"
		if err := os.WriteFile(replacementPath, append(data, '\n'), 0o600); err != nil {
			return err
		}
		if err := os.Rename(replacementPath, wakeTargetPath(root, "codex")); err != nil {
			return err
		}
		removed, err := removeWakeTargetIfSnapshotMatchesAt(
			dirfd,
			agentDir,
			root,
			"codex",
			snapshot,
		)
		if err == nil || !strings.Contains(err.Error(), "changed before cleanup") {
			return errors.New("replacement wake target was not rejected")
		}
		if removed {
			return errors.New("replacement wake target was removed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists || persisted.Created != "2026-07-23T02:00:00Z" {
		t.Fatalf("replacement wake target was not preserved: target=%#v exists=%v err=%v", persisted, exists, err)
	}
}

func TestAcquireAuthoritativeWakeClaimPublishesOwnerAndCleanupPreservesLifetime(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-acquire-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner

	ownerState := wakeOwnerSame
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if err := validateAuthoritativeWakeOwner(got); err != nil {
			t.Fatalf("observed invalid owner %#v: %v", got, err)
		}
		if ownerState == wakeOwnerSame {
			observation := liveWakeOwnerObservationForTest()
			observation.Reason = "test owner evidence"
			return observation, nil
		}
		return wakeOwnerObservation{State: ownerState, Reason: "test owner evidence"}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	wakeRunning := true
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != os.Getpid() {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    wakeRunning,
			StartToken: "67890",
			BootID:     owner.BootID,
			Executable: "/usr/local/bin/amq",
			Args:       []string{"amq", "wake", "--me", "codex", "--root", root},
		}
	})

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatalf("acquire owner claim: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != wakeOwnerLockFileMode {
		t.Fatalf("owner lock mode = %o, want %o", got, wakeOwnerLockFileMode)
	}
	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockValid || inspection.Lock.OwnerSchema != wakeOwnerLockSchema ||
		inspection.Lock.WakeMode != wakeOwnerWakeMode || !sameWakeOwner(inspection.Lock.Owner, &owner) {
		t.Fatalf("owner inspection = %#v", inspection)
	}
	ownerState = wakeOwnerUnknown
	if err := writeWakeReadyFile(root, "codex", filepath.Join(root, "owner.ready"), inspection); err == nil ||
		!strings.Contains(err.Error(), "owner") {
		t.Fatalf("readiness with unknown owner error = %v, want owner refusal", err)
	}
	if current := inspectWakeLock(root, "codex"); !sameWakeLockGeneration(inspection, current) {
		t.Fatal("failed owner readiness validation changed claim")
	}
	ownerState = wakeOwnerSame
	readyPath := filepath.Join(root, "owner.ready")
	if err := writeWakeReadyFile(root, "codex", readyPath, inspection); err != nil {
		t.Fatalf("write owner readiness: %v", err)
	}
	differentReadyOwner := owner
	differentReadyOwner.PID++
	differentReadyOwner.ProcessStart = "23456"
	if _, err := validateWakeReadyFileAgainstOwner(
		root,
		"codex",
		readyPath,
		&differentReadyOwner,
	); err == nil || !strings.Contains(err.Error(), "requested owner") {
		t.Fatalf("different requested owner readiness error = %v", err)
	}
	if err := writeWakePreparedFile(root, "codex", inspection); err != nil {
		t.Fatalf("write owner prepared marker: %v", err)
	}
	preparedInfo, err := os.Lstat(wakePreparedPath(root, "codex"))
	if err != nil || preparedInfo.Mode().Perm() != 0o600 {
		t.Fatalf("owner prepared marker info=%v err=%v", preparedInfo, err)
	}

	cleanup()
	after := inspectWakeLock(root, "codex")
	if !sameWakeLockGeneration(inspection, after) {
		t.Fatalf("ordinary wake cleanup removed owner claim: before=%#v after=%#v", inspection, after)
	}

	_, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		acceptExistingValid: true,
		target:              &target,
		wakeMode:            wakeTargetInjectVia,
	})
	var alreadyRunning *wakeAlreadyRunningError
	if !errors.As(err, &alreadyRunning) || !sameWakeLockGeneration(inspection, alreadyRunning.Inspection) {
		t.Fatalf("same-owner reuse error = %v, want exact existing generation", err)
	}

	differentTarget := target
	differentOwner := owner
	differentOwner.PID++
	differentOwner.ProcessStart = "23456"
	differentTarget.Owner = &differentOwner
	_, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		acceptExistingValid: true,
		target:              &differentTarget,
		wakeMode:            wakeTargetInjectVia,
	})
	if err == nil || !strings.Contains(err.Error(), "owned by live process") {
		t.Fatalf("different-owner acquisition error = %v, want live-owner conflict", err)
	}

	wakeRunning = false
	_, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		acceptExistingValid: true,
		target:              &target,
		wakeMode:            wakeTargetInjectVia,
	})
	if err == nil || !strings.Contains(err.Error(), "unusable wake") {
		t.Fatalf("same-owner damaged wake error = %v, want recover-owner refusal", err)
	}
	preserved := inspectWakeLock(root, "codex")
	if !sameWakeLockGeneration(inspection, preserved) {
		t.Fatalf("same-owner damaged wake changed claim: before=%#v after=%#v", inspection, preserved)
	}
}

func TestConcurrentAuthoritativeAcquisitionPublishesOneGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-contention-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "67890",
			BootID:     owner.BootID,
			Executable: "/usr/local/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		}
	})
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
		return wakeOwnerObservation{State: wakeOwnerSame}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	const contenders = 16
	start := make(chan struct{})
	results := make(chan error, contenders)
	var group sync.WaitGroup
	for range contenders {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			requested := target
			_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				acceptExistingValid: true,
				target:              &requested,
				wakeMode:            wakeTargetInjectVia,
			})
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)

	winners := 0
	reusers := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		var alreadyRunning *wakeAlreadyRunningError
		if errors.As(err, &alreadyRunning) {
			reusers++
			continue
		}
		t.Fatalf("contention result: %v", err)
	}
	if winners != 1 || reusers != contenders-1 {
		t.Fatalf("contention winners=%d reusers=%d, want 1/%d", winners, reusers, contenders-1)
	}
	inspection := inspectWakeLock(root, "codex")
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		_, err := validateAuthoritativeWakeClaimPairAt(dirfd, agentDir, inspection)
		return err
	})
	_ = agentDir.Close()
	if err != nil {
		t.Fatalf("contended claim is incomplete: %v", err)
	}
}

func TestAcquireAuthoritativeWakeClaimReclaimsOnlyADeadOwnerWithAbsentWake(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-takeover-injector")
	oldOwner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    88,
	}
	oldTarget := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	oldTarget.Owner = &oldOwner
	oldLock := bindWakeLockToTarget(wakeLock{
		PID:          5151,
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		Started:      "2026-07-23T00:00:00Z",
		ProcessStart: "67890",
		BootID:       oldOwner.BootID,
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--me", "codex", "--root", root},
		Generation:   "old-owner-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &oldOwner,
	}, oldTarget)
	oldLock.WakeMode = wakeOwnerWakeMode
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, "codex", oldTarget, oldLock)
	}); err != nil {
		t.Fatal(err)
	}
	_ = agentDir.Close()

	newOwner := wakeOwner{
		PID:          6262,
		ProcessStart: "23456",
		BootID:       oldOwner.BootID,
		SessionID:    99,
	}
	newTarget := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	newTarget.Owner = &newOwner

	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(owner wakeOwner) (wakeOwnerObservation, error) {
		switch owner {
		case oldOwner:
			return wakeOwnerObservation{State: wakeOwnerDead, Reason: "owner process is not running"}, nil
		case newOwner:
			return wakeOwnerObservation{State: wakeOwnerSame}, nil
		default:
			t.Fatalf("unexpected owner observation: %#v", owner)
			return wakeOwnerObservation{}, nil
		}
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case oldLock.PID:
			return wakeProcessInfo{PID: pid}
		case os.Getpid():
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "78901",
				BootID:     newOwner.BootID,
				Executable: "/usr/local/bin/amq",
				Args:       []string{"amq", "wake", "--me", "codex", "--root", root, "--inject-via", injector},
			}
		default:
			return wakeProcessInfo{PID: pid}
		}
	})

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &newTarget,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatalf("dead-owner takeover: %v", err)
	}
	defer cleanup()
	inspection := inspectWakeLock(root, "codex")
	if inspection.Lock.Generation == oldLock.Generation ||
		!sameWakeOwner(inspection.Lock.Owner, &newOwner) ||
		inspection.Lock.TargetDigest != mustWakeTargetDigest(newTarget) {
		t.Fatalf("takeover claim = %#v", inspection)
	}
}

func TestGenericStaleCleanupCannotRemoveAuthoritativeOwnerClaim(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-cleanup-fence-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	lock := bindWakeLockToTarget(wakeLock{
		PID:          5151,
		TTY:          "unknown",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Generation:   "owner-cleanup-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath := writeWakeLockForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockStale {
		t.Fatalf("owner wake status = %s, want stale wake process", inspection.Status)
	}
	if err := validateWakeLockStaleRemoval(inspection); err == nil || !strings.Contains(err.Error(), "recover-owner") {
		t.Fatalf("generic stale-removal gate = %v, want recover-owner refusal", err)
	}
	if err := cleanupTerminatedWakeLock(inspection); err == nil || !strings.Contains(err.Error(), "recover-owner") {
		t.Fatalf("generic terminated cleanup = %v, want recover-owner refusal", err)
	}
	after := inspectWakeLock(root, "codex")
	if !sameWakeLockGeneration(inspection, after) {
		t.Fatalf("generic cleanup changed owner claim: before=%#v after=%#v", inspection, after)
	}
}

func TestExactHelperCleanupHandlesFreshOwnerlessFallback(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "ownerless-fallback-cleanup-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	const wakePID = 5151
	lock := bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "67890",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		Generation:   "ownerless-fallback-cleanup-generation",
	}, target)
	lock.WakeMode = wakeTargetInjectVia
	writeWakeLockForTest(t, root, "codex", lock)

	stopped := false
	closed := false
	capability := &authoritativeWakeChildCapability{
		stop: func() error {
			stopped = true
			return nil
		},
		close: func() error {
			closed = true
			return nil
		},
	}
	waiter := &wakeProcessWaiter{done: make(chan struct{})}
	close(waiter.done)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		if stopped {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: lock.ProcessStart,
			BootID:     lock.BootID,
			Executable: lock.Executable,
			Args:       lock.Args,
		}
	})
	owner := wakeOwner{
		PID:          os.Getpid(),
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	if err := terminateAuthoritativeWakeHelperProcess(
		&os.Process{Pid: wakePID},
		waiter,
		capability,
		root,
		"codex",
		owner,
	); err != nil {
		t.Fatal(err)
	}
	if !stopped || !closed {
		t.Fatalf("stable fallback cleanup stopped=%v closed=%v", stopped, closed)
	}
	if inspectWakeLock(root, "codex").Exists {
		t.Fatal("stable fallback cleanup left the generic lock")
	}
}

func TestExactHelperCleanupRollsBackOnlyCurrentAuthoritativeOwner(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	owner, err := captureAuthoritativeCurrentWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "authoritative-rollback-cleanup-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner
	const wakePID = 5151
	lock := bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		Generation:   "authoritative-rollback-cleanup-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, "codex", target, lock)
	})
	_ = agentDir.Close()
	if err != nil {
		t.Fatal(err)
	}

	stopped := false
	capability := &authoritativeWakeChildCapability{
		stop: func() error {
			stopped = true
			return nil
		},
	}
	waiter := &wakeProcessWaiter{done: make(chan struct{})}
	close(waiter.done)
	realInspect := inspectWakeProcess
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			if stopped {
				return wakeProcessInfo{
					PID:        pid,
					Running:    true,
					StartToken: "78901",
					BootID:     lock.BootID,
					Executable: lock.Executable,
					Args:       lock.Args,
				}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: lock.ProcessStart,
				BootID:     lock.BootID,
				Executable: lock.Executable,
				Args:       lock.Args,
			}
		}
		return realInspect(pid)
	})
	if err := terminateAuthoritativeWakeHelperProcess(
		&os.Process{Pid: wakePID},
		waiter,
		capability,
		root,
		"codex",
		owner,
	); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("authoritative rollback did not use the stable child capability")
	}
	if inspectWakeLock(root, "codex").Exists {
		t.Fatal("exact current-owner rollback left the authoritative lock")
	}

	stopped = false
	agentDir, err = openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, "codex", target, lock)
	})
	_ = agentDir.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackAuthoritativeWakeClaim(root, "codex", owner); err == nil ||
		!strings.Contains(err.Error(), "not conclusively absent") {
		t.Fatalf("live recorded wake rollback error = %v, want refusal", err)
	}
	if inspection := inspectWakeLock(root, "codex"); inspection.Status != wakeLockValid ||
		!inspection.IdentityConfirmed {
		t.Fatalf("refused live rollback changed authoritative claim: %#v", inspection)
	}
}

func TestRecoverOwnerRequiresExactTokenAndCallerSessionForLiveOwner(t *testing.T) {
	tests := []struct {
		name        string
		ownerState  wakeOwnerIdentityState
		token       bool
		callerSID   int
		callerErr   error
		wantSuccess bool
		wantReason  string
	}{
		{
			name:        "live owner exact token and session releases",
			ownerState:  wakeOwnerSame,
			token:       true,
			callerSID:   99,
			wantSuccess: true,
		},
		{
			name:       "token replay from another session refuses",
			ownerState: wakeOwnerSame,
			token:      true,
			callerSID:  100,
			wantReason: "OS session",
		},
		{
			name:       "caller session lookup failure refuses",
			ownerState: wakeOwnerSame,
			token:      true,
			callerErr:  errors.New("session unavailable"),
			wantReason: "session unavailable",
		},
		{
			name:       "missing live owner token refuses",
			ownerState: wakeOwnerSame,
			callerSID:  99,
			wantReason: "token",
		},
		{
			name:        "dead owner needs no token",
			ownerState:  wakeOwnerDead,
			wantSuccess: true,
		},
		{
			name:       "unknown owner preserves claim",
			ownerState: wakeOwnerUnknown,
			wantReason: "unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			injector := writeExecutableForTest(t, "owner-recover-injector")
			owner := wakeOwner{
				PID:          4242,
				ProcessStart: "12345",
				BootID:       "11111111-1111-1111-1111-111111111111",
				SessionID:    99,
			}
			target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
			target.Owner = &owner
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatal(err)
			}
			lock := bindWakeLockToTarget(wakeLock{
				PID:          5151,
				TTY:          "unknown",
				ProcessStart: "67890",
				BootID:       owner.BootID,
				Generation:   "owner-recover-generation",
				OwnerSchema:  wakeOwnerLockSchema,
				Owner:        &owner,
			}, target)
			lock.WakeMode = wakeOwnerWakeMode
			lockPath := writeWakeLockForTest(t, root, "codex", lock)
			if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
				t.Fatal(err)
			}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return wakeProcessInfo{PID: pid}
			})
			oldObserve := observeAuthoritativeWakeOwner
			observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
				if got != owner {
					t.Fatalf("observed owner = %#v, want %#v", got, owner)
				}
				return wakeOwnerObservation{State: test.ownerState, Reason: test.wantReason}, nil
			}
			t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
			stubWakeProcessSID(t, func(pid int) (int, error) {
				if pid != os.Getpid() {
					t.Fatalf("caller sid lookup pid = %d, want %d", pid, os.Getpid())
				}
				return test.callerSID, test.callerErr
			})
			if test.token {
				encoded, err := encodeWakeOwnerEnv(owner)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv(envWakeOwner, encoded)
			} else {
				t.Setenv(envWakeOwner, "")
			}

			result, err := recoverOwnerWake(root, "codex")
			if test.wantSuccess {
				if err != nil || result.Status != "recovered" {
					t.Fatalf("recover result = %#v err=%v", result, err)
				}
				if inspectWakeLock(root, "codex").Exists {
					t.Fatal("successful recovery preserved owner lock")
				}
				return
			}
			if err == nil || result.Status != "refused" ||
				!strings.Contains(strings.ToLower(result.Reason), strings.ToLower(test.wantReason)) {
				t.Fatalf("recover result = %#v err=%v, want reason %q", result, err, test.wantReason)
			}
			if !inspectWakeLock(root, "codex").Exists {
				t.Fatal("refused recovery removed owner lock")
			}
		})
	}
}

func TestRecoverOwnerUsesAuthoritativeLockEvidenceWhenTargetIsMissing(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-missing-target-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner
	lock := bindWakeLockToTarget(wakeLock{
		PID:          5151,
		TTY:          "unknown",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Generation:   "owner-missing-target-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath := writeWakeLockForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
		return wakeOwnerObservation{State: wakeOwnerDead}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	result, err := recoverOwnerWake(root, "codex")
	if err != nil || result.Status != "recovered" {
		t.Fatalf("missing-target recovery result = %#v err=%v", result, err)
	}
	if inspectWakeLock(root, "codex").Exists {
		t.Fatal("missing-target recovery preserved authoritative lock")
	}
}

func TestRecoverOwnerPreservesMalformedAuthoritativeLockEnvelope(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*wakeLock)
	}{
		{name: "root mismatch", mutate: func(lock *wakeLock) { lock.Root += "-other" }},
		{name: "agent mismatch", mutate: func(lock *wakeLock) { lock.Agent = "claude" }},
		{name: "invalid wake pid", mutate: func(lock *wakeLock) { lock.PID = 0 }},
		{name: "malformed owner token", mutate: func(lock *wakeLock) { lock.Owner.ProcessStart = "not-a-platform-token" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			injector := writeExecutableForTest(t, "owner-malformed-envelope-injector")
			owner := wakeOwner{
				PID:          4242,
				ProcessStart: "12345",
				BootID:       "11111111-1111-1111-1111-111111111111",
				SessionID:    99,
			}
			target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
			target.Owner = &owner
			lock := bindWakeLockToTarget(wakeLock{
				PID:          5151,
				TTY:          "unknown",
				ProcessStart: "67890",
				BootID:       owner.BootID,
				Generation:   "owner-malformed-envelope-generation",
				OwnerSchema:  wakeOwnerLockSchema,
				Owner:        &owner,
			}, target)
			lock.WakeMode = wakeOwnerWakeMode
			test.mutate(&lock)
			lockPath := writeWakeLockForTest(t, root, "codex", lock)
			if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			oldObserve := observeAuthoritativeWakeOwner
			observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
				t.Fatal("malformed lock envelope reached owner observation")
				return wakeOwnerObservation{}, nil
			}
			t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

			result, err := recoverOwnerWake(root, "codex")
			if err == nil || result.Status != "refused" {
				t.Fatalf("malformed-envelope recovery = %#v err=%v", result, err)
			}
			after, readErr := os.ReadFile(lockPath)
			afterInfo, statErr := os.Lstat(lockPath)
			if readErr != nil || statErr != nil || !bytes.Equal(before, after) || !os.SameFile(beforeInfo, afterInfo) {
				t.Fatalf("malformed envelope changed: read=%v stat=%v", readErr, statErr)
			}
		})
	}
}

func TestRecoverOwnerPreservesUntrustedTargetButRejectsDifferentCompleteOwner(t *testing.T) {
	for _, test := range []struct {
		name          string
		different     bool
		wantRecovered bool
	}{
		{name: "corrupt target is preserved", wantRecovered: true},
		{name: "different complete owner is ambiguous", different: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			injector := writeExecutableForTest(t, "owner-untrusted-target-injector")
			owner := wakeOwner{
				PID:          4242,
				ProcessStart: "12345",
				BootID:       "11111111-1111-1111-1111-111111111111",
				SessionID:    99,
			}
			target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
			target.Owner = &owner
			lock := bindWakeLockToTarget(wakeLock{
				PID:          5151,
				TTY:          "unknown",
				ProcessStart: "67890",
				BootID:       owner.BootID,
				Generation:   "owner-untrusted-target-generation",
				OwnerSchema:  wakeOwnerLockSchema,
				Owner:        &owner,
			}, target)
			lock.WakeMode = wakeOwnerWakeMode
			lockPath := writeWakeLockForTest(t, root, "codex", lock)
			if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
				t.Fatal(err)
			}
			targetPath := wakeTargetPath(root, "codex")
			if test.different {
				other := owner
				other.PID++
				other.ProcessStart = "23456"
				target.Owner = &other
				data, err := json.Marshal(target)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(targetPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(targetPath, []byte("{not-json"), 0o600); err != nil {
				t.Fatal(err)
			}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return wakeProcessInfo{PID: pid}
			})
			oldObserve := observeAuthoritativeWakeOwner
			observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
				return wakeOwnerObservation{State: wakeOwnerDead}, nil
			}
			t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

			result, err := recoverOwnerWake(root, "codex")
			if test.wantRecovered {
				if err != nil || result.Status != "recovered" {
					t.Fatalf("untrusted-target recovery = %#v err=%v", result, err)
				}
				if inspectWakeLock(root, "codex").Exists {
					t.Fatal("untrusted target prevented authoritative lock recovery")
				}
				if _, err := os.Lstat(targetPath); err != nil {
					t.Fatalf("untrusted target was not preserved: %v", err)
				}
				return
			}
			if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "different owners") {
				t.Fatalf("different-owner recovery = %#v err=%v", result, err)
			}
			if !inspectWakeLock(root, "codex").Exists {
				t.Fatal("ambiguous different-owner target allowed lock removal")
			}
		})
	}
}

func TestDecideOwnerLifecycleTransitionPreemptsLegacyMutationRules(t *testing.T) {
	tests := []struct {
		name     string
		evidence wakeOwnerTransitionEvidence
		want     wakeOwnerTransitionAction
	}{
		{
			name: "fresh exact owner publishes",
			evidence: wakeOwnerTransitionEvidence{
				Request:        wakeOwnerRequestAcquire,
				Claim:          wakeClaimAbsent,
				RequestedState: wakeOwnerSame,
			},
			want: wakeOwnerActionPublish,
		},
		{
			name: "same owner reuses exact usable wake",
			evidence: wakeOwnerTransitionEvidence{
				Request:        wakeOwnerRequestAcquire,
				Claim:          wakeClaimAuthoritative,
				RequestedState: wakeOwnerSame,
				PersistedState: wakeOwnerSame,
				OwnersEqual:    true,
				WakeUsable:     true,
			},
			want: wakeOwnerActionReuse,
		},
		{
			name: "same owner damaged wake requires explicit recovery",
			evidence: wakeOwnerTransitionEvidence{
				Request:        wakeOwnerRequestAcquire,
				Claim:          wakeClaimAuthoritative,
				RequestedState: wakeOwnerSame,
				PersistedState: wakeOwnerSame,
				OwnersEqual:    true,
			},
			want: wakeOwnerActionRefuse,
		},
		{
			name: "different live owner conflicts",
			evidence: wakeOwnerTransitionEvidence{
				Request:        wakeOwnerRequestAcquire,
				Claim:          wakeClaimAuthoritative,
				RequestedState: wakeOwnerSame,
				PersistedState: wakeOwnerSame,
			},
			want: wakeOwnerActionRefuse,
		},
		{
			name: "dead owner with exact live wake stops before release",
			evidence: wakeOwnerTransitionEvidence{
				Request:            wakeOwnerRequestAcquire,
				Claim:              wakeClaimAuthoritative,
				RequestedState:     wakeOwnerSame,
				PersistedState:     wakeOwnerDead,
				WakeExactStoppable: true,
			},
			want: wakeOwnerActionStopAndRelease,
		},
		{
			name: "dead owner with absent wake releases",
			evidence: wakeOwnerTransitionEvidence{
				Request:        wakeOwnerRequestAcquire,
				Claim:          wakeClaimAuthoritative,
				RequestedState: wakeOwnerSame,
				PersistedState: wakeOwnerDead,
				WakeAbsent:     true,
			},
			want: wakeOwnerActionRelease,
		},
		{
			name: "unknown owner never aliases dead",
			evidence: wakeOwnerTransitionEvidence{
				Request:        wakeOwnerRequestAcquire,
				Claim:          wakeClaimAuthoritative,
				RequestedState: wakeOwnerSame,
				PersistedState: wakeOwnerUnknown,
				WakeAbsent:     true,
			},
			want: wakeOwnerActionRefuse,
		},
		{
			name: "generic acquisition cannot cross owner claim",
			evidence: wakeOwnerTransitionEvidence{
				Request: wakeGenericRequestAcquire,
				Claim:   wakeClaimAuthoritative,
			},
			want: wakeOwnerActionRefuse,
		},
		{
			name: "generic acquisition stays on the absent legacy path",
			evidence: wakeOwnerTransitionEvidence{
				Request: wakeGenericRequestAcquire,
				Claim:   wakeClaimAbsent,
			},
			want: wakeOwnerActionLegacy,
		},
		{
			name: "generic cleanup cannot cross owner claim",
			evidence: wakeOwnerTransitionEvidence{
				Request: wakeGenericRequestMutate,
				Claim:   wakeClaimAuthoritative,
			},
			want: wakeOwnerActionRefuse,
		},
		{
			name: "generic cleanup stays on the ownerless legacy path",
			evidence: wakeOwnerTransitionEvidence{
				Request: wakeGenericRequestMutate,
				Claim:   wakeClaimGeneric,
			},
			want: wakeOwnerActionLegacy,
		},
		{
			name: "authenticated live recovery stops exact wake",
			evidence: wakeOwnerTransitionEvidence{
				Request:            wakeOwnerRequestRecover,
				Claim:              wakeClaimAuthoritative,
				PersistedState:     wakeOwnerSame,
				Authenticated:      true,
				WakeExactStoppable: true,
			},
			want: wakeOwnerActionStopAndRelease,
		},
		{
			name: "token without caller authentication cannot recover",
			evidence: wakeOwnerTransitionEvidence{
				Request:        wakeOwnerRequestRecover,
				Claim:          wakeClaimAuthoritative,
				PersistedState: wakeOwnerSame,
				WakeAbsent:     true,
			},
			want: wakeOwnerActionRefuse,
		},
		{
			name: "corruption is never downgraded",
			evidence: wakeOwnerTransitionEvidence{
				Request:        wakeOwnerRequestAcquire,
				Claim:          wakeClaimInvalid,
				RequestedState: wakeOwnerSame,
			},
			want: wakeOwnerActionRefuse,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := decideOwnerLifecycleTransition(test.evidence); got != test.want {
				t.Fatalf("transition = %s, want %s", got, test.want)
			}
		})
	}
}

func TestClassifyAuthoritativeWakeOwnerKeepsDeadSeparateFromUnknown(t *testing.T) {
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	matching := wakeProcessInfo{
		PID:        owner.PID,
		Running:    true,
		StartToken: owner.ProcessStart,
		BootID:     owner.BootID,
	}

	tests := []struct {
		name       string
		process    wakeProcessInfo
		sessionID  int
		sessionErr error
		want       wakeOwnerIdentityState
	}{
		{
			name:       "conclusive absence wins without boot evidence",
			process:    wakeProcessInfo{PID: owner.PID},
			sessionErr: errors.New("session unavailable"),
			want:       wakeOwnerDead,
		},
		{
			name:      "present process without boot is unknown",
			process:   wakeProcessInfo{PID: owner.PID, Running: true, StartToken: owner.ProcessStart},
			sessionID: owner.SessionID,
			want:      wakeOwnerUnknown,
		},
		{
			name:      "matching exact identity is same",
			process:   matching,
			sessionID: owner.SessionID,
			want:      wakeOwnerSame,
		},
		{
			name: "comparable boot mismatch proves dead",
			process: wakeProcessInfo{
				PID:        owner.PID,
				Running:    true,
				StartToken: owner.ProcessStart,
				BootID:     "22222222-2222-2222-2222-222222222222",
			},
			sessionID: owner.SessionID,
			want:      wakeOwnerDead,
		},
		{
			name: "start mismatch after boot match proves dead",
			process: wakeProcessInfo{
				PID:        owner.PID,
				Running:    true,
				StartToken: "replacement-start",
				BootID:     owner.BootID,
			},
			sessionID: owner.SessionID,
			want:      wakeOwnerDead,
		},
		{
			name:      "session mismatch on same process is unknown",
			process:   matching,
			sessionID: owner.SessionID + 1,
			want:      wakeOwnerUnknown,
		},
		{
			name:       "session inspection failure is unknown",
			process:    matching,
			sessionErr: errors.New("permission denied"),
			want:       wakeOwnerUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, _ := classifyAuthoritativeWakeOwner(owner, test.process, test.sessionID, test.sessionErr)
			if state != test.want {
				t.Fatalf("state = %s, want %s", state, test.want)
			}
		})
	}
}

func TestClassifyStableAuthoritativeWakeOwnerRejectsChangedSecondSnapshot(t *testing.T) {
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	matching := wakeProcessInfo{
		PID:        owner.PID,
		Running:    true,
		StartToken: owner.ProcessStart,
		BootID:     owner.BootID,
	}

	tests := []struct {
		name      string
		first     wakeProcessInfo
		firstSID  int
		second    wakeProcessInfo
		secondSID int
		want      wakeOwnerIdentityState
	}{
		{
			name:      "two exact snapshots are same",
			first:     matching,
			firstSID:  owner.SessionID,
			second:    matching,
			secondSID: owner.SessionID,
			want:      wakeOwnerSame,
		},
		{
			name:      "process changes between snapshots",
			first:     matching,
			firstSID:  owner.SessionID,
			second:    wakeProcessInfo{PID: owner.PID},
			secondSID: owner.SessionID,
			want:      wakeOwnerUnknown,
		},
		{
			name:      "session changes between snapshots",
			first:     matching,
			firstSID:  owner.SessionID,
			second:    matching,
			secondSID: owner.SessionID + 1,
			want:      wakeOwnerUnknown,
		},
		{
			name:      "two conclusive absence snapshots are dead",
			first:     wakeProcessInfo{PID: owner.PID},
			second:    wakeProcessInfo{PID: owner.PID},
			secondSID: owner.SessionID,
			want:      wakeOwnerDead,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _ := classifyStableAuthoritativeWakeOwner(
				owner,
				test.first, test.firstSID, nil,
				test.second, test.secondSID, nil,
			)
			if got != test.want {
				t.Fatalf("stable classification = %s, want %s", got, test.want)
			}
		})
	}
}
