package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/wakelock"
)

// The lock record, its reader, and its validators live in internal/wakelock;
// these names keep the wake state machine on the one implementation.
type (
	wakeLock           = wakelock.Lock
	wakeLockStatus     = wakelock.Status
	wakeLockFileReader = wakelock.FileReader
)

const (
	wakeLockFileName      = wakelock.FileName
	wakeOwnerLockSchema   = wakelock.OwnerSchema
	wakeOwnerWakeMode     = wakelock.OwnerWakeMode
	wakeOwnerLockFileMode = wakelock.OwnerFileMode
)

const (
	wakeLockMissing    = wakelock.Missing
	wakeLockValid      = wakelock.Valid
	wakeLockStale      = wakelock.Stale
	wakeLockCreating   = wakelock.Creating
	wakeLockUnverified = wakelock.Unverified
)

func readWakeLockFileWithInfo(path string) ([]byte, os.FileInfo, error) {
	return wakelock.ReadFile(path)
}

func validateWakeLockFile(path string, info os.FileInfo) error {
	return wakelock.ValidateFile(path, info)
}

func validateWakeLockFormat(lock wakeLock, info os.FileInfo) error {
	return wakelock.ValidateFormat(lock, info)
}

func validateWakeLockStateBinding(lock wakeLock) error {
	return wakelock.ValidateStateBinding(lock)
}

func validateAuthoritativeWakeProcessIdentity(lock wakeLock) error {
	return wakelock.ValidateAuthoritativeProcessIdentity(lock)
}

func canonicalizeWakeRoot(root string) (string, error) {
	return wakelock.CanonicalizeRoot(root)
}

type wakeProcessInfo struct {
	PID                       int
	Running                   bool
	StartToken                string
	BootID                    string
	LegacyBootID              string
	Executable                string
	ExecutablePath            string
	Args                      []string
	ControllingTerminalKnown  bool
	HasControllingTerminal    bool
	ControllingTerminalDevice int32
	InspectError              error
}

type wakeIdentityState uint8

const (
	wakeIdentityUnknown wakeIdentityState = iota
	wakeIdentitySame
	wakeIdentityGoneOrDifferent
)

type wakeOwnerIdentityState uint8

const (
	wakeOwnerUnknown wakeOwnerIdentityState = iota
	wakeOwnerSame
	wakeOwnerDead
)

func (state wakeOwnerIdentityState) String() string {
	switch state {
	case wakeOwnerSame:
		return "same"
	case wakeOwnerDead:
		return "dead"
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
	observationErr    error
	decodeErr         error
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
	lockPath := filepath.Join(fsq.AgentBase(root, me), wakeLockFileName)
	return inspectWakeLockWithReader(root, me, lockPath, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileWithInfo(lockPath)
	})
}

func inspectWakeLockWithReader(root, me, lockPath string, read wakeLockFileReader) wakeLockInspection {
	inspection := readWakeLockMetadataWithReader(root, me, lockPath, read)
	if !inspection.Exists || inspection.Status != wakeLockMissing {
		return inspection
	}
	inspection.Process = inspectWakeProcess(inspection.Lock.PID)
	classifyWakeLock(root, me, &inspection)
	return inspection
}

func readWakeLockMetadataWithReader(root, me, lockPath string, read wakeLockFileReader) wakeLockInspection {
	observation := wakelock.Observe(root, read)
	return wakeLockInspection{
		Exists:         observation.Exists,
		Status:         observation.Status,
		Reason:         observation.Reason,
		Root:           observation.Root,
		Agent:          me,
		LockPath:       lockPath,
		PID:            observation.Lock.PID,
		Lock:           observation.Lock,
		raw:            observation.Raw,
		fileInfo:       observation.FileInfo,
		observationErr: observation.ObservationErr,
		decodeErr:      observation.DecodeErr,
	}
}

func classifyWakeLock(root, me string, inspection *wakeLockInspection) {
	lock := inspection.Lock
	if err := validateWakeLockFormat(lock, inspection.fileInfo); err != nil {
		inspection.Status = wakeLockUnverified
		inspection.Reason = err.Error()
		return
	}
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
	lockRoot, err := canonicalizeWakeRoot(lock.Root)
	if err != nil {
		inspection.Status = wakeLockUnverified
		inspection.Reason = fmt.Sprintf("canonicalize lock root: %v", err)
		return
	}
	inspectRoot, err := canonicalizeWakeRoot(root)
	if err != nil {
		inspection.Status = wakeLockUnverified
		inspection.Reason = fmt.Sprintf("canonicalize root: %v", err)
		return
	}
	if lockRoot != inspectRoot {
		inspection.Status = wakeLockStale
		inspection.Reason = "root mismatch"
		return
	}
	if lock.Agent != "" && lock.Agent != me {
		inspection.Status = wakeLockStale
		inspection.Reason = "agent mismatch"
		return
	}
	if machineState, machineReason := classifyWakeLockMachine(lock); machineState != wakeMachineSame {
		inspection.Status = wakeLockUnverified
		inspection.Reason = machineReason
		return
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

func validateWakeLockInspectionStateBindingJSON(inspection wakeLockInspection) error {
	if inspection.decodeErr != nil {
		return fmt.Errorf("wake lock JSON fields are malformed: %w", inspection.decodeErr)
	}
	if inspection.Status == wakeLockCreating || inspection.raw == nil {
		return nil
	}
	return wakelock.ValidateStateBindingJSON(inspection.raw)
}

func sameWakeOwner(first, second *wakeOwner) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return *first == *second
}

func validateWakeLockRepairable(inspection wakeLockInspection) error {
	if err := validateGenericWakeLifecycleTransition(inspection, wakeGenericRequestMutate); err != nil {
		return err
	}
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

func wakeLockHasOwnerMarkers(inspection wakeLockInspection) bool {
	if inspection.Lock.OwnerSchema != 0 || inspection.Lock.Owner != nil || inspection.Lock.WakeMode == wakeOwnerWakeMode {
		return true
	}
	return inspection.fileInfo != nil && inspection.fileInfo.Mode().Perm() == wakeOwnerLockFileMode
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

func removeWakeLockIfUnchangedGuardedWithIOStatus(
	inspection wakeLockInspection,
	read wakeLockFileReader,
	remove func() error,
) (bool, error) {
	if err := validateGenericWakeLifecycleTransition(inspection, wakeGenericRequestMutate); err != nil {
		return false, err
	}
	current, currentInfo, err := read()
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("re-read wake lock before removal: %w", err)
	}
	if !bytes.Equal(current, inspection.raw) {
		return false, fmt.Errorf("wake lock changed while cleaning stale lock; retry")
	}
	if inspection.fileInfo == nil || currentInfo == nil || !sameWakeFileIdentity(inspection.fileInfo, currentInfo) {
		return false, fmt.Errorf("wake lock generation changed while cleaning stale lock; retry")
	}
	multipleLinks, linkErr := wakeLockHasMultipleLinks(currentInfo)
	if linkErr != nil {
		return false, fmt.Errorf("cannot determine wake lock hard-link count; preserving it: %w", linkErr)
	}
	if multipleLinks {
		return false, fmt.Errorf("wake lock has multiple hard links; preserving it to avoid mutating an alias")
	}
	// Pathname removal is safe under the lifecycle guard held by every
	// cooperating writer; an unguarded same-UID writer is out of contract. A
	// rename-and-verify alternative would expose lock absence to pre-P2b readers
	// during a two-step removal, creating a real competing-authority hazard.
	if err := remove(); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove stale wake lock: %w", err)
	}
	return true, nil
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
	canonical, err := canonicalizeWakeRoot(root)
	if err != nil {
		return ""
	}
	return canonical
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
	strongProcessIdentityMatched := false
	if lock.WakeMode == wakeOwnerWakeMode {
		if err := validateAuthoritativeWakeProcessIdentity(lock); err != nil {
			return wakeIdentityUnknown, err.Error()
		}
	}
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
		strongProcessIdentityMatched =
			lock.BootID != "" && bootComparison == bootIDMatch
	}
	if proc.Executable == "" {
		return wakeIdentityUnknown, inspectionReason("process identity unavailable", proc.InspectError)
	}
	if strongProcessIdentityMatched && !isAMQExecutable(proc.Executable) {
		return wakeIdentityUnknown, "matching process identity has a non-amq executable name"
	}
	if !processLooksLikeAMQ(proc) {
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

func classifyAuthoritativeWakeOwner(owner wakeOwner, proc wakeProcessInfo, sessionID int, sessionErr error) (wakeOwnerIdentityState, string) {
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		return wakeOwnerUnknown, err.Error()
	}
	if !proc.Running {
		return wakeOwnerDead, "owner process is not running"
	}
	if proc.StartToken == "" {
		return wakeOwnerUnknown, inspectionReason("owner process start unavailable", proc.InspectError)
	}
	switch compareWakeBootID(owner.BootID, proc) {
	case bootIDMismatch:
		return wakeOwnerDead, "owner boot id changed"
	case bootIDUnknown:
		return wakeOwnerUnknown, "owner boot id unavailable or incomparable"
	}
	if proc.StartToken != owner.ProcessStart {
		return wakeOwnerDead, "owner process start changed"
	}
	if sessionErr != nil {
		return wakeOwnerUnknown, fmt.Sprintf("owner session unavailable: %v", sessionErr)
	}
	if sessionID <= 0 {
		return wakeOwnerUnknown, "owner session unavailable"
	}
	if sessionID != owner.SessionID {
		return wakeOwnerUnknown, "owner session changed"
	}
	return wakeOwnerSame, ""
}

func classifyStableAuthoritativeWakeOwner(
	owner wakeOwner,
	first wakeProcessInfo,
	firstSessionID int,
	firstSessionErr error,
	second wakeProcessInfo,
	secondSessionID int,
	secondSessionErr error,
) (wakeOwnerIdentityState, string) {
	firstState, firstReason := classifyAuthoritativeWakeOwner(owner, first, firstSessionID, firstSessionErr)
	secondState, secondReason := classifyAuthoritativeWakeOwner(owner, second, secondSessionID, secondSessionErr)
	if firstState == wakeOwnerDead && secondState == wakeOwnerDead {
		return wakeOwnerDead, firstReason
	}
	if firstState != wakeOwnerSame || secondState != wakeOwnerSame {
		if firstState != secondState {
			return wakeOwnerUnknown, "owner identity changed while inspecting"
		}
		if firstReason != "" {
			return wakeOwnerUnknown, firstReason
		}
		return wakeOwnerUnknown, secondReason
	}
	if !sameWakeOwnerProcessSnapshot(first, second) || firstSessionID != secondSessionID {
		return wakeOwnerUnknown, "owner identity changed while inspecting"
	}
	return wakeOwnerSame, ""
}

func sameWakeOwnerProcessSnapshot(first, second wakeProcessInfo) bool {
	return first.PID == second.PID &&
		first.Running == second.Running &&
		first.StartToken == second.StartToken &&
		first.BootID == second.BootID &&
		first.LegacyBootID == second.LegacyBootID
}

type bootIDComparison int

const (
	bootIDMatch bootIDComparison = iota
	bootIDMismatch
	bootIDUnknown
)

// compareWakeBootID applies launch.CompareBootID to the current boot id and
// to the legacy boottime form a wake recorded before it used UUIDs.
func compareWakeBootID(recorded string, proc wakeProcessInfo) bootIDComparison {
	if recorded == "" {
		return bootIDMatch
	}
	for _, current := range []string{proc.BootID, proc.LegacyBootID} {
		if current != "" && launch.CompareBootID(recorded, current) == launch.BootIDMatch {
			return bootIDMatch
		}
	}
	if launch.CompareBootID(recorded, proc.BootID) == launch.BootIDMismatch {
		return bootIDMismatch
	}
	return bootIDUnknown
}
