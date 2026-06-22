package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	WakeMode     string   `json:"wake_mode,omitempty"`     // Injection mode that created repair metadata
	TargetDigest string   `json:"target_digest,omitempty"` // Binds .wake.target to this lock instance
}

type wakeProcessInfo struct {
	PID          int
	Running      bool
	StartToken   string
	BootID       string
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

	data, err := readWakeLockFile(lockPath)
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

func readWakeLockFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateWakeLockFile(path, info); err != nil {
		return nil, err
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat wake lock: %w", err)
	}
	if err := validateWakeLockFile(path, openedInfo); err != nil {
		return nil, err
	}
	return readWakeMetadata(file, "wake lock", path)
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

	proc := inspection.Process
	if !proc.Running {
		inspection.Status = wakeLockStale
		inspection.Reason = "pid not running"
		return
	}
	if lock.ProcessStart != "" {
		if proc.StartToken == "" {
			inspection.Status = wakeLockUnverified
			inspection.Reason = inspectionReason("process start time unavailable", proc.InspectError)
			return
		}
		if lock.BootID != "" && proc.BootID != "" && lock.BootID != proc.BootID {
			inspection.Status = wakeLockStale
			inspection.Reason = "boot id mismatch"
			return
		}
		if lock.ProcessStart != proc.StartToken {
			inspection.Status = wakeLockStale
			inspection.Reason = "process start time mismatch"
			return
		}
	}
	if proc.Executable == "" {
		inspection.Status = wakeLockUnverified
		inspection.Reason = inspectionReason("process identity unavailable", proc.InspectError)
		return
	}
	if !processLooksLikeAMQ(proc) {
		inspection.Status = wakeLockStale
		inspection.Reason = "pid is not amq"
		return
	}
	if len(proc.Args) > 0 && !processArgsLookLikeWake(proc.Args) {
		inspection.Status = wakeLockStale
		inspection.Reason = "pid is not amq wake"
		return
	}

	if lock.ProcessStart != "" {
		inspection.IdentityConfirmed = true
		inspection.Status = wakeLockValid
		return
	}

	if wakeArgsMatchRootAgent(proc.Args, root, me) {
		inspection.IdentityConfirmed = true
		inspection.Status = wakeLockValid
		return
	}

	inspection.Status = wakeLockUnverified
	inspection.Reason = "legacy lock lacks process start metadata"
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
	// Identity-token mismatches are removable only when process inspection proves
	// the live PID is not an amq wake. Other stale reasons keep the historical
	// self-heal behavior for corrupt or structurally stale lock files.
	switch inspection.Reason {
	case "boot id mismatch", "process start time mismatch":
		if wakeProcessProvenNotWake(inspection.Process) {
			return nil
		}
		return fmt.Errorf("wake lock stale reason %q is not removable while pid %d may still be amq wake", inspection.Reason, inspection.PID)
	default:
		return nil
	}
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
	current, err := readWakeLockFile(inspection.LockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("re-read wake lock before removal: %w", err)
	}
	if !bytes.Equal(current, inspection.raw) {
		return fmt.Errorf("wake lock changed while cleaning stale lock; retry")
	}
	if err := os.Remove(inspection.LockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale wake lock: %w", err)
	}
	return nil
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
	if lock.BootID != "" && proc.BootID != "" && lock.BootID != proc.BootID {
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

func wakeProcessStillMatches(inspection wakeLockInspection) bool {
	proc := inspectWakeProcess(inspection.PID)
	if !proc.Running {
		return false
	}
	if inspection.Lock.ProcessStart != "" {
		if proc.StartToken == "" || inspection.Lock.ProcessStart != proc.StartToken {
			return false
		}
		if inspection.Lock.BootID != "" && proc.BootID != "" && inspection.Lock.BootID != proc.BootID {
			return false
		}
	}
	if proc.Executable == "" || !processLooksLikeAMQ(proc) {
		return false
	}
	if len(proc.Args) > 0 && !processArgsLookLikeWake(proc.Args) {
		return false
	}
	if inspection.Lock.ProcessStart == "" && !wakeArgsMatchRootAgent(proc.Args, inspection.Root, inspection.Agent) {
		return false
	}
	return true
}
