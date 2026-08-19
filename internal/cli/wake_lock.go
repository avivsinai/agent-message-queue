package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// wakeLock represents the lock file content for wake process deduplication.
type wakeLock struct {
	PID                  int                  `json:"pid"`
	TTY                  string               `json:"tty"`
	Root                 string               `json:"root"`                             // Absolute path to disambiguate relative AM_ROOT
	Agent                string               `json:"agent,omitempty"`                  // Agent handle that owns this lock
	Hostname             string               `json:"hostname,omitempty"`               // Host that created the lock; diagnostic, drifts with the network on macOS
	MachineID            string               `json:"machine_id,omitempty"`             // Stable machine identity that created the lock
	Started              string               `json:"started"`                          // Wall-clock diagnostic timestamp
	ProcessStart         string               `json:"process_start,omitempty"`          // Kernel process start token, guards PID reuse
	BootID               string               `json:"boot_id,omitempty"`                // Boot identity paired with ProcessStart when available
	Executable           string               `json:"executable,omitempty"`             // Diagnostic process executable basename/path
	Args                 []string             `json:"args,omitempty"`                   // Diagnostic argv when available
	ImagePath            string               `json:"image_path,omitempty"`             // Resolved executable path captured by the wake itself
	ImageVersion         string               `json:"image_version,omitempty"`          // AMQ version embedded in the running wake image
	WakeMode             string               `json:"wake_mode,omitempty"`              // none, raw, paste, or inject-via; empty means a legacy pre-v0.44 lock
	TargetDigest         string               `json:"target_digest,omitempty"`          // Binds .wake.target to this lock instance
	Generation           string               `json:"generation,omitempty"`             // Random nonce binding readiness and exact cleanup to this instance
	StateGeneration      string               `json:"state_generation,omitempty"`       // P2b binding to the state target section for this exact lock generation
	StateDigest          string               `json:"state_digest,omitempty"`           // Durable P2b wire slot; equals TargetDigest in v1
	SourceGeneration     string               `json:"source_generation,omitempty"`      // Dead generation inherited by a repaired wake
	SourceFloorDigest    string               `json:"source_floor_digest,omitempty"`    // Exact repair floor inherited by a repaired wake
	ControlSocket        string               `json:"control_socket,omitempty"`         // Generation-derived cooperative control endpoint
	OwnerSchema          int                  `json:"owner_schema,omitempty"`           // Non-zero only for an authoritative owner-bound lock
	Owner                *wakeOwner           `json:"owner,omitempty"`                  // Exact owner identity for an authoritative owner-bound lock
	ResumeSchema         int                  `json:"resume_schema,omitempty"`          // Agent-safe self-exec protocol; independent of OwnerSchema
	ResumeOwner          *wakeOwner           `json:"resume_owner,omitempty"`           // Exact coop owner used only for reload authentication/lifetime
	ResumeSignal         string               `json:"resume_signal,omitempty"`          // Same-process restart request signal advertised by resumable wakes
	RunningImageEvidence *wakeImageEvidenceV1 `json:"running_image_evidence,omitempty"` // Serialized running-image authority
}

const (
	wakeOwnerLockSchema   = 1
	wakeOwnerWakeMode     = "owner-inject-via-v1"
	wakeOwnerLockFileMode = os.FileMode(0o400)
)

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

type wakeLockFileReader func() ([]byte, os.FileInfo, error)

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
	inspection := wakeLockInspection{
		Status:   wakeLockMissing,
		Agent:    me,
		LockPath: lockPath,
	}
	canonicalRoot, err := canonicalizeWakeRoot(root)
	if err != nil {
		inspection.Status = wakeLockUnverified
		inspection.Reason = fmt.Sprintf("canonicalize root: %v", err)
		inspection.observationErr = err
		return inspection
	}
	inspection.Root = canonicalRoot

	data, fileInfo, err := read()
	if err != nil {
		if os.IsNotExist(err) {
			return inspection
		}
		inspection.Exists = true
		inspection.Status = wakeLockUnverified
		inspection.Reason = fmt.Sprintf("cannot read lock: %v", err)
		inspection.observationErr = err
		return inspection
	}

	inspection.Exists = true
	inspection.raw = data
	inspection.fileInfo = fileInfo
	if json.Valid(data) {
		if err := validateWakeLockStateBindingFieldsJSON(data); err != nil {
			inspection.Status = wakeLockUnverified
			inspection.Reason = err.Error()
			return inspection
		}
	}
	var existing wakeLock
	if err := json.Unmarshal(data, &existing); err != nil {
		if json.Valid(data) {
			inspection.Status = wakeLockUnverified
			inspection.Reason = fmt.Sprintf("wake lock JSON fields are malformed: %v", err)
			inspection.decodeErr = err
			return inspection
		}
		if fileInfo != nil && fileInfo.Mode().Perm() == wakeOwnerLockFileMode {
			inspection.Status = wakeLockUnverified
			inspection.Reason = "wake owner schema is malformed; owner-bound lock may be from a newer amq"
			return inspection
		}
		if fileInfo != nil && time.Since(fileInfo.ModTime()) < 2*time.Second {
			inspection.Status = wakeLockCreating
			inspection.Reason = "lock is being created"
			return inspection
		}
		inspection.Status = wakeLockUnverified
		inspection.Reason = fmt.Sprintf("wake lock JSON is malformed: %v", err)
		return inspection
	}

	inspection.Lock = existing
	inspection.PID = existing.PID
	if err := validateWakeLockStateBindingJSON(data); err != nil {
		inspection.Status = wakeLockUnverified
		inspection.Reason = err.Error()
		return inspection
	}
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
		return nil, nil, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake lock %s changed while opening", path),
		)
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
	if got := info.Mode().Perm(); got != 0o600 && got != wakeOwnerLockFileMode {
		return fmt.Errorf("wake lock %s mode is %o, want 0600 or 0400", path, got)
	}
	return validateWakeTargetPathOwnership("wake lock", path, info)
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

func validateWakeLockFormat(lock wakeLock, info os.FileInfo) error {
	if info == nil {
		return fmt.Errorf("wake lock file identity unavailable")
	}
	if err := validateWakeLockStateBinding(lock); err != nil {
		return err
	}
	switch info.Mode().Perm() {
	case wakeOwnerLockFileMode:
		if lock.OwnerSchema != wakeOwnerLockSchema {
			return fmt.Errorf("wake owner schema %d unsupported; owner-bound lock may be from a newer amq", lock.OwnerSchema)
		}
		if lock.WakeMode != wakeOwnerWakeMode {
			return fmt.Errorf("wake owner mode %q unsupported; owner-bound lock may be from a newer amq", lock.WakeMode)
		}
		if lock.Owner == nil {
			return fmt.Errorf("wake owner identity missing")
		}
		if err := validateAuthoritativeWakeOwner(*lock.Owner); err != nil {
			return fmt.Errorf("wake owner identity invalid: %w", err)
		}
		if err := validateAuthoritativeWakeProcessIdentity(lock); err != nil {
			return err
		}
		if strings.TrimSpace(lock.TargetDigest) == "" {
			return fmt.Errorf("wake owner target digest missing")
		}
		if strings.TrimSpace(lock.Generation) == "" {
			return fmt.Errorf("wake owner generation missing")
		}
	case 0o600:
		if lock.OwnerSchema != 0 || lock.Owner != nil || lock.WakeMode == wakeOwnerWakeMode {
			return fmt.Errorf("wake owner markers require mode 0400")
		}
	default:
		return fmt.Errorf("wake lock mode %o unsupported", info.Mode().Perm())
	}
	return nil
}

func validateWakeLockStateBinding(lock wakeLock) error {
	hasGeneration := lock.StateGeneration != ""
	hasDigest := lock.StateDigest != ""
	if hasGeneration != hasDigest {
		return fmt.Errorf("wake state_generation and state_digest must be present together")
	}
	if !hasGeneration {
		return nil
	}
	if !validWakeStateGeneration(lock.StateGeneration) {
		return fmt.Errorf("wake state generation is invalid")
	}
	if !validWakeStateDigest(lock.StateDigest) {
		return fmt.Errorf("wake state digest is invalid")
	}
	if lock.StateGeneration != lock.Generation {
		return fmt.Errorf("wake state generation does not match lock generation")
	}
	if lock.TargetDigest == "" {
		return fmt.Errorf("wake state binding requires a target digest")
	}
	if lock.StateDigest != lock.TargetDigest {
		return fmt.Errorf("wake state digest does not match lock target digest")
	}
	if lock.WakeMode != wakeTargetInjectVia && lock.WakeMode != wakeOwnerWakeMode {
		return fmt.Errorf("wake state binding requires a target-bearing lock")
	}
	return nil
}

// Wake-lock input trust is enforced jointly by the reader's syntax gate, the
// occurrence-preserving scanner below, json.Unmarshal's known-field type
// checks, and the envelope validators. Keep that complete family synchronized
// with TestWakeLockJSONTrustMatrix: (1) raw parse failure; (2) non-object top
// levels; (3) known-field wrong types at early and late byte positions;
// (4) direct and folded known-field nulls; (5) top-level duplicate known keys
// in both orders and through folded aliases; (6) nested colliding keys, which
// are opaque; (7) single, duplicate, and null unknown fields, which remain
// additive ABI; and (8) valid bound and unbound controls.
func validateWakeLockStateBindingJSON(data []byte) error {
	fields, err := decodeWakeLockJSONFields(data)
	if err != nil {
		return err
	}
	if err := validateWakeLockStateBindingFields(fields); err != nil {
		return err
	}
	pid, exists := fields["pid"]
	if !exists {
		return fmt.Errorf("wake lock JSON is missing required field %q", "pid")
	}
	var decodedPID int
	if bytes.Equal(bytes.TrimSpace(pid), []byte("null")) || json.Unmarshal(pid, &decodedPID) != nil {
		return fmt.Errorf("wake lock JSON field %q must be an integer", "pid")
	}
	for _, name := range []string{"tty", "root", "started"} {
		raw, exists := fields[name]
		if !exists {
			return fmt.Errorf("wake lock JSON is missing required field %q", name)
		}
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return fmt.Errorf("wake lock JSON field %q must be a string", name)
		}
	}
	return nil
}

func validateWakeLockStateBindingFieldsJSON(data []byte) error {
	fields, err := decodeWakeLockJSONFields(data)
	if err != nil {
		return err
	}
	return validateWakeLockStateBindingFields(fields)
}

// decodeWakeLockJSONFields preserves top-level member occurrences before map
// or struct decoding can collapse them. Only top-level known names are
// uniqueness constrained; values and unknown fields remain opaque here.
func decodeWakeLockJSONFields(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("wake lock JSON is malformed: %w", err)
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("wake lock JSON must be an object")
	}

	fields := make(map[string]json.RawMessage)
	knownNames := make(map[string]string)
	knownOrder := make([]string, 0, len(wakeLockKnownJSONFields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("wake lock JSON is malformed: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("wake lock JSON field name is malformed")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("wake lock JSON field %q is malformed: %w", name, err)
		}

		canonicalName, known := wakeLockJSONFieldCanonicalName(name)
		if !known {
			// Unknown additive fields are opaque. In particular, duplicate
			// unknown names remain compatible with future serializers.
			fields[name] = raw
			continue
		}
		if previous, exists := knownNames[canonicalName]; exists {
			return nil, fmt.Errorf("wake lock JSON field %q duplicates known field %q", name, previous)
		}
		knownNames[canonicalName] = name
		knownOrder = append(knownOrder, canonicalName)
		fields[canonicalName] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("wake lock JSON is malformed: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("wake lock JSON has trailing data")
		}
		return nil, fmt.Errorf("wake lock JSON is malformed: %w", err)
	}
	for _, canonicalName := range knownOrder {
		if bytes.Equal(bytes.TrimSpace(fields[canonicalName]), []byte("null")) {
			return nil, fmt.Errorf("wake lock JSON field %q must not be null", knownNames[canonicalName])
		}
	}
	return fields, nil
}

func validateWakeLockStateBindingFields(fields map[string]json.RawMessage) error {
	generation, hasGeneration := fields["state_generation"]
	digest, hasDigest := fields["state_digest"]
	if hasGeneration != hasDigest {
		return fmt.Errorf("wake state_generation and state_digest must be present together")
	}
	if hasGeneration {
		if _, err := decodeWakeLockStateBindingString("state_generation", generation); err != nil {
			return err
		}
		if _, err := decodeWakeLockStateBindingString("state_digest", digest); err != nil {
			return err
		}
	}
	return nil
}

func validateWakeLockInspectionStateBindingJSON(inspection wakeLockInspection) error {
	if inspection.decodeErr != nil {
		return fmt.Errorf("wake lock JSON fields are malformed: %w", inspection.decodeErr)
	}
	if inspection.Status == wakeLockCreating || inspection.raw == nil {
		return nil
	}
	return validateWakeLockStateBindingJSON(inspection.raw)
}

var wakeLockKnownJSONFields = func() []string {
	typ := reflect.TypeFor[wakeLock]()
	fields := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		name := strings.Split(typ.Field(index).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields = append(fields, name)
		}
	}
	return fields
}()

func wakeLockJSONFieldCanonicalName(name string) (string, bool) {
	for _, known := range wakeLockKnownJSONFields {
		if strings.EqualFold(name, known) {
			return known, true
		}
	}
	return "", false
}

func decodeWakeLockStateBindingString(name string, raw json.RawMessage) (string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("wake %s must be a non-empty string", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", fmt.Errorf("wake %s must be a non-empty string", name)
	}
	return value, nil
}

func validateAuthoritativeWakeProcessIdentity(lock wakeLock) error {
	if err := validateAuthoritativeWakeOwnerToken("wake process start", lock.ProcessStart); err != nil {
		return err
	}
	if err := validateAuthoritativeWakeOwnerToken("wake boot id", lock.BootID); err != nil {
		return err
	}
	if !validWakeOwnerProcessStart(lock.ProcessStart) {
		return fmt.Errorf("wake process start has malformed platform value")
	}
	if !validWakeOwnerBootID(lock.BootID) {
		return fmt.Errorf("wake boot id has malformed platform value")
	}
	return nil
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

func validateWakeLockStaleRemoval(inspection wakeLockInspection) error {
	if _, err := readWakeStateSelectionForInspection(
		inspection.Root,
		inspection.Agent,
		inspection,
	); err != nil {
		return err
	}
	if wakeLockHasOwnerMarkers(inspection) {
		return fmt.Errorf("owner-bound wake claims require 'amq wake recover-owner --me %s'", inspection.Agent)
	}
	if err := validateWakeLockRepairable(inspection); err == nil {
		return nil
	} else if inspection.Status != wakeLockStale {
		return err
	}
	// Identity mismatches reach stale only when the tri-state classifier has
	// affirmative proof that the recorded generation is gone or different.
	return nil
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

func removeWakeLockIfUnchanged(inspection wakeLockInspection) error {
	return withWakeLifecycleGuard(inspection.Root, inspection.Agent, func() error {
		return removeWakeLockIfUnchangedGuarded(inspection)
	})
}

func removeWakeLockIfUnchangedGuarded(inspection wakeLockInspection) error {
	if _, err := readWakeStateSelectionForInspection(
		inspection.Root,
		inspection.Agent,
		inspection,
	); err != nil {
		return err
	}
	if err := reclaimWakeRestartStateForGuardedLockRemoval(inspection); err != nil {
		return fmt.Errorf("reconcile wake restart ownership before lock removal: %w", err)
	}
	committed, err := removeWakeLockIfUnchangedGuardedWithIOStatus(
		inspection,
		func() ([]byte, os.FileInfo, error) { return readWakeLockFileWithInfo(inspection.LockPath) },
		func() error { return os.Remove(inspection.LockPath) },
	)
	if err != nil || !committed {
		return err
	}
	if err := removeWakeSelfUpgradeDiagnosticGuarded(inspection.Root, inspection.Agent); err != nil {
		_ = writeStderr(
			"warning: removed wake lock for %s but left diagnostic-only self-upgrade residue: %v\n",
			inspection.Agent,
			err,
		)
	}
	return nil
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

func canonicalizeWakeRoot(root string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err == nil {
		return filepath.Clean(realRoot), nil
	}
	if isUncertainSymlinkResolution(err) {
		return "", fmt.Errorf("canonicalize wake root %q: %w", absRoot, err)
	}
	return filepath.Clean(absRoot), nil
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
