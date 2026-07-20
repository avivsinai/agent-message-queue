package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// wakeLock represents the lock file content for wake process deduplication.
type wakeLock struct {
	PID          int      `json:"pid"`
	TTY          string   `json:"tty"`
	Root         string   `json:"root"`                    // Absolute path to disambiguate relative AM_ROOT
	Agent        string   `json:"agent,omitempty"`         // Agent handle that owns this lock
	Hostname     string   `json:"hostname,omitempty"`      // Host that created the lock
	Started      string   `json:"started"`                 // Wall-clock diagnostic timestamp
	ProcessStart string   `json:"process_start,omitempty"` // Kernel process start token, guards PID reuse
	BootID       string   `json:"boot_id,omitempty"`       // Boot identity paired with ProcessStart when available
	Executable   string   `json:"executable,omitempty"`    // Diagnostic process executable basename/path
	Args         []string `json:"args,omitempty"`          // Diagnostic argv when available
	WakeMode     string   `json:"wake_mode,omitempty"`     // none, raw, paste, or inject-via; empty means a legacy pre-v0.44 lock
	TargetDigest string   `json:"target_digest,omitempty"` // Binds .wake.target to this lock instance
	Generation   string   `json:"generation,omitempty"`    // Random nonce binding readiness and exact cleanup to this instance
}

type wakeProcessInfo struct {
	PID          int
	Running      bool
	StartToken   string
	BootID       string
	LegacyBootID string
	Executable   string
	Args         []string
	InspectError error
}

type wakeLockStatus string

const (
	wakeLockMissing    wakeLockStatus = "missing"
	wakeLockValid      wakeLockStatus = "valid"
	wakeLockStale      wakeLockStatus = "stale"
	wakeLockCreating   wakeLockStatus = "creating"
	wakeLockUnverified wakeLockStatus = "unverified"
)

type wakeIdentityState uint8

const (
	wakeIdentityUnknown wakeIdentityState = iota
	wakeIdentitySame
	wakeIdentityGoneOrDifferent
)

func (state wakeIdentityState) String() string {
	switch state {
	case wakeIdentitySame:
		return "same"
	case wakeIdentityGoneOrDifferent:
		return "gone or different"
	default:
		return "unknown"
	}
}

type wakeLockInspection struct {
	Exists            bool
	Status            wakeLockStatus
	Reason            string
	Root              string
	Agent             string
	LockPath          string
	PID               int
	Lock              wakeLock
	Process           wakeProcessInfo
	IdentityConfirmed bool
	raw               []byte
	fileInfo          os.FileInfo
}

var inspectWakeProcess = inspectWakeProcessPlatform

type wakeAlreadyRunningError struct {
	Agent      string
	Inspection wakeLockInspection
}

func (e *wakeAlreadyRunningError) Error() string {
	lock := e.Inspection.Lock
	return fmt.Sprintf("wake already running for %s (pid %d on %s since %s)",
		e.Agent, lock.PID, lock.TTY, lock.Started)
}

func inspectWakeLock(root, me string) wakeLockInspection {
	lockPath := filepath.Join(fsq.AgentBase(root, me), ".wake.lock")
	inspection := wakeLockInspection{
		Status:   wakeLockMissing,
		Root:     canonicalWakeRoot(root),
		Agent:    me,
		LockPath: lockPath,
	}

	data, fileInfo, err := readWakeLockFileWithInfo(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return inspection
		}
		inspection.Exists = true
		inspection.Status = wakeLockUnverified
		inspection.Reason = fmt.Sprintf("cannot read lock: %v", err)
		return inspection
	}

	inspection.Exists = true
	inspection.raw = data
	inspection.fileInfo = fileInfo
	var existing wakeLock
	if err := json.Unmarshal(data, &existing); err != nil {
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) < 2*time.Second {
			inspection.Status = wakeLockCreating
			inspection.Reason = "lock is being created"
			return inspection
		}
		inspection.Status = wakeLockStale
		inspection.Reason = "invalid lock json"
		return inspection
	}

	inspection.Lock = existing
	inspection.PID = existing.PID
	inspection.Process = inspectWakeProcess(existing.PID)
	classifyWakeLock(root, me, &inspection)
	return inspection
}

func readWakeLockFileWithInfo(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := validateWakeLockFile(path, info); err != nil {
		return nil, nil, err
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat wake lock: %w", err)
	}
	if err := validateWakeLockFile(path, openedInfo); err != nil {
		return nil, nil, err
	}
	if !os.SameFile(info, openedInfo) {
		return nil, nil, fmt.Errorf("wake lock %s changed while opening", path)
	}
	data, err := readWakeMetadata(file, "wake lock", path)
	return data, openedInfo, err
}

func validateWakeLockFile(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wake lock %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake lock %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("wake lock %s mode is %o, want 0600", path, got)
	}
	return validateWakeTargetPathOwnership("wake lock", path, info)
}

func classifyWakeLock(root, me string, inspection *wakeLockInspection) {
	lock := inspection.Lock
	if lock.PID <= 0 {
		inspection.Status = wakeLockStale
		inspection.Reason = "invalid pid"
		return
	}
	if strings.TrimSpace(lock.Root) == "" {
		inspection.Status = wakeLockStale
		inspection.Reason = "lock root missing"
		return
	}
	if canonicalWakeRoot(lock.Root) != canonicalWakeRoot(root) {
		inspection.Status = wakeLockStale
		inspection.Reason = "root mismatch"
		return
	}
	if lock.Agent != "" && lock.Agent != me {
		inspection.Status = wakeLockStale
		inspection.Reason = "agent mismatch"
		return
	}
	if lock.Hostname != "" {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			inspection.Status = wakeLockUnverified
			inspection.Reason = inspectionReason("hostname unavailable", err)
			return
		}
		if lock.Hostname != hostname {
			inspection.Status = wakeLockUnverified
			inspection.Reason = "hostname mismatch"
			return
		}
	}

	state, reason := classifyWakeIdentity(*inspection, inspection.Process)
	inspection.Reason = reason
	switch state {
	case wakeIdentitySame:
		inspection.IdentityConfirmed = true
		inspection.Status = wakeLockValid
	case wakeIdentityGoneOrDifferent:
		inspection.Status = wakeLockStale
	default:
		inspection.Status = wakeLockUnverified
	}
}

func validateWakeLockRepairable(inspection wakeLockInspection) error {
	if inspection.Status != wakeLockStale {
		return fmt.Errorf("wake lock status %q is not repairable", inspection.Status)
	}
	switch inspection.Reason {
	case "pid not running", "pid is not amq", "pid is not amq wake":
		return nil
	default:
		return fmt.Errorf("wake lock stale reason %q is not repairable", inspection.Reason)
	}
}

func validateWakeLockStaleRemoval(inspection wakeLockInspection) error {
	if err := validateWakeLockRepairable(inspection); err == nil {
		return nil
	} else if inspection.Status != wakeLockStale {
		return err
	}
	// Identity mismatches reach stale only when the tri-state classifier has
	// affirmative proof that the recorded generation is gone or different.
	return nil
}

func wakeProcessProvenNotWake(proc wakeProcessInfo) bool {
	if !proc.Running {
		return true
	}
	if proc.Executable == "" && len(proc.Args) == 0 {
		return false
	}
	if !processLooksLikeAMQ(proc) {
		return true
	}
	return len(proc.Args) > 0 && !processArgsLookLikeWake(proc.Args)
}

func removeWakeLockIfUnchanged(inspection wakeLockInspection) error {
	return withWakeLifecycleGuard(inspection.Root, inspection.Agent, func() error {
		return removeWakeLockIfUnchangedGuarded(inspection)
	})
}

func removeWakeLockIfUnchangedGuarded(inspection wakeLockInspection) error {
	current, currentInfo, err := readWakeLockFileWithInfo(inspection.LockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("re-read wake lock before removal: %w", err)
	}
	if !bytes.Equal(current, inspection.raw) {
		return fmt.Errorf("wake lock changed while cleaning stale lock; retry")
	}
	if inspection.fileInfo == nil || currentInfo == nil || !sameWakeFileIdentity(inspection.fileInfo, currentInfo) {
		return fmt.Errorf("wake lock generation changed while cleaning stale lock; retry")
	}
	if err := os.Remove(inspection.LockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale wake lock: %w", err)
	}
	return nil
}

func sameWakeLockGeneration(first, second wakeLockInspection) bool {
	if !first.Exists || !second.Exists || first.fileInfo == nil || second.fileInfo == nil {
		return false
	}
	if !sameWakeFileIdentity(first.fileInfo, second.fileInfo) || !bytes.Equal(first.raw, second.raw) {
		return false
	}
	if first.Lock.Generation != "" || second.Lock.Generation != "" {
		return first.Lock.Generation != "" && first.Lock.Generation == second.Lock.Generation
	}
	return true
}

func currentWakeLockMatches(lock wakeLock) bool {
	if lock.PID != os.Getpid() {
		return false
	}
	if lock.ProcessStart == "" {
		return true
	}
	proc := inspectWakeProcess(os.Getpid())
	if !proc.Running || proc.StartToken == "" {
		return false
	}
	if compareWakeBootID(lock.BootID, proc) != bootIDMatch {
		return false
	}
	return lock.ProcessStart == proc.StartToken
}

func canonicalWakeRoot(root string) string {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	if realRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = realRoot
	}
	return filepath.Clean(absRoot)
}

func wakeLockAlreadyRunningError(me string, inspection wakeLockInspection) error {
	return &wakeAlreadyRunningError{
		Agent:      me,
		Inspection: inspection,
	}
}

func inspectionReason(base string, err error) string {
	if err == nil {
		return base
	}
	return fmt.Sprintf("%s: %v", base, err)
}

func processLooksLikeAMQ(proc wakeProcessInfo) bool {
	if isAMQExecutable(proc.Executable) {
		return true
	}
	if len(proc.Args) > 0 && isAMQExecutable(proc.Args[0]) {
		return true
	}
	return false
}

func processArgsLookLikeWake(args []string) bool {
	if len(args) < 2 {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "wake" {
			return true
		}
	}
	return false
}

func wakeArgsMatchRootAgent(args []string, root, me string) bool {
	if !processArgsLookLikeWake(args) {
		return false
	}
	rootMatch := false
	meMatch := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--root" && i+1 < len(args):
			rootMatch = canonicalWakeRoot(args[i+1]) == canonicalWakeRoot(root)
			i++
		case strings.HasPrefix(arg, "--root="):
			rootMatch = canonicalWakeRoot(strings.TrimPrefix(arg, "--root=")) == canonicalWakeRoot(root)
		case arg == "--me" && i+1 < len(args):
			meMatch = args[i+1] == me
			i++
		case strings.HasPrefix(arg, "--me="):
			meMatch = strings.TrimPrefix(arg, "--me=") == me
		}
	}
	return rootMatch && meMatch
}

func isAMQExecutable(value string) bool {
	base := filepath.Base(strings.Trim(value, `"'`))
	return base == "amq"
}

func inspectWakeIdentity(inspection wakeLockInspection) wakeIdentityState {
	state, _ := classifyWakeIdentity(inspection, inspectWakeProcess(inspection.PID))
	return state
}

func classifyWakeIdentity(inspection wakeLockInspection, proc wakeProcessInfo) (wakeIdentityState, string) {
	lock := inspection.Lock
	if !proc.Running {
		return wakeIdentityGoneOrDifferent, "pid not running"
	}
	if lock.ProcessStart != "" {
		if proc.StartToken == "" {
			return wakeIdentityUnknown, inspectionReason("process start time unavailable", proc.InspectError)
		}
		bootComparison := compareWakeBootID(lock.BootID, proc)
		switch bootComparison {
		case bootIDMismatch:
			return wakeIdentityGoneOrDifferent, "boot id mismatch"
		case bootIDUnknown:
			if wakeProcessProvenNotWake(proc) {
				return wakeIdentityGoneOrDifferent, "boot id mismatch"
			}
			return wakeIdentityUnknown, "boot id mismatch"
		}
		if lock.ProcessStart != proc.StartToken {
			if lock.BootID == "" {
				if wakeProcessProvenNotWake(proc) {
					return wakeIdentityGoneOrDifferent, "process start time mismatch"
				}
				return wakeIdentityUnknown, "process start time mismatch"
			}
			return wakeIdentityGoneOrDifferent, "process start time mismatch"
		}
	}
	if proc.Executable == "" || !processLooksLikeAMQ(proc) {
		if proc.Executable == "" {
			return wakeIdentityUnknown, inspectionReason("process identity unavailable", proc.InspectError)
		}
		return wakeIdentityGoneOrDifferent, "pid is not amq"
	}
	if len(proc.Args) > 0 && !processArgsLookLikeWake(proc.Args) {
		return wakeIdentityGoneOrDifferent, "pid is not amq wake"
	}
	if lock.ProcessStart != "" {
		return wakeIdentitySame, ""
	}
	if lock.BootID != "" {
		return wakeIdentityUnknown, "boot id requires process start metadata"
	}
	if wakeArgsMatchRootAgent(proc.Args, inspection.Root, inspection.Agent) {
		return wakeIdentitySame, ""
	}
	return wakeIdentityUnknown, "legacy lock lacks process start metadata"
}

type bootIDComparison int

const (
	bootIDMatch bootIDComparison = iota
	bootIDMismatch
	bootIDUnknown
)

func compareWakeBootID(recorded string, proc wakeProcessInfo) bootIDComparison {
	if recorded == "" {
		return bootIDMatch
	}
	for _, current := range []string{proc.BootID, proc.LegacyBootID} {
		if current == "" {
			continue
		}
		if strings.EqualFold(recorded, current) {
			return bootIDMatch
		}
		if legacyDarwinBootIDsMatch(recorded, current) {
			return bootIDMatch
		}
	}
	// Only unlike boots of the same identity representation are conclusive.
	// A UUID cannot be disproved by a readable boottime, and vice versa.
	if isDarwinBootUUID(recorded) && isDarwinBootUUID(proc.BootID) {
		return bootIDMismatch
	}
	return bootIDUnknown
}

func wakeBootIDMismatch(recorded string, proc wakeProcessInfo) bool {
	return compareWakeBootID(recorded, proc) == bootIDMismatch
}

func isDarwinBootUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// Legacy Darwin boot IDs came from kern.boottime, which can move slightly as
// macOS corrects wall-clock time. A one-second migration tolerance preserves
// old live wake locks without making two realistically distinct boots equal.
func legacyDarwinBootIDsMatch(first, second string) bool {
	firstTime, firstOK := parseLegacyDarwinBootID(first)
	secondTime, secondOK := parseLegacyDarwinBootID(second)
	if !firstOK || !secondOK {
		return false
	}
	secDelta := firstTime.Unix() - secondTime.Unix()
	if secDelta < -1 || secDelta > 1 {
		return false
	}
	return firstTime.Sub(secondTime) <= time.Second && secondTime.Sub(firstTime) <= time.Second
}

func parseLegacyDarwinBootID(value string) (time.Time, bool) {
	seconds, nanos, ok := strings.Cut(value, ".")
	if !ok || seconds == "" || len(nanos) != 9 || strings.Contains(nanos, ".") {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	nsec, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil || nsec < 0 || nsec >= int64(time.Second) {
		return time.Time{}, false
	}
	return time.Unix(sec, nsec), true
}
