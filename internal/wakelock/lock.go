// Package wakelock reads and validates the wake lock that `amq wake` writes
// under an agent mailbox. It never writes, removes, or publishes a lock.
package wakelock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

// Lock represents the lock file content for wake process deduplication.
type Lock struct {
	PID                  int                        `json:"pid"`
	TTY                  string                     `json:"tty"`
	Root                 string                     `json:"root"`                             // Absolute path to disambiguate relative AM_ROOT
	Agent                string                     `json:"agent,omitempty"`                  // Agent handle that owns this lock
	Hostname             string                     `json:"hostname,omitempty"`               // Host that created the lock; diagnostic, drifts with the network on macOS
	MachineID            string                     `json:"machine_id,omitempty"`             // Stable machine identity that created the lock
	Started              string                     `json:"started"`                          // Wall-clock diagnostic timestamp
	ProcessStart         string                     `json:"process_start,omitempty"`          // Kernel process start token, guards PID reuse
	BootID               string                     `json:"boot_id,omitempty"`                // Boot identity paired with ProcessStart when available
	Executable           string                     `json:"executable,omitempty"`             // Diagnostic process executable basename/path
	Args                 []string                   `json:"args,omitempty"`                   // Diagnostic argv when available
	ImagePath            string                     `json:"image_path,omitempty"`             // Resolved executable path captured by the wake itself
	ImageVersion         string                     `json:"image_version,omitempty"`          // AMQ version embedded in the running wake image
	WakeMode             string                     `json:"wake_mode,omitempty"`              // none, raw, paste, or inject-via; empty means a legacy pre-v0.44 lock
	TargetDigest         string                     `json:"target_digest,omitempty"`          // Binds .wake.target to this lock instance
	Generation           string                     `json:"generation,omitempty"`             // Random nonce binding readiness and exact cleanup to this instance
	StateGeneration      string                     `json:"state_generation,omitempty"`       // P2b binding to the state target section for this exact lock generation
	StateDigest          string                     `json:"state_digest,omitempty"`           // Durable P2b wire slot; equals TargetDigest in v1
	SourceGeneration     string                     `json:"source_generation,omitempty"`      // Dead generation inherited by a repaired wake
	SourceFloorDigest    string                     `json:"source_floor_digest,omitempty"`    // Exact repair floor inherited by a repaired wake
	ControlSocket        string                     `json:"control_socket,omitempty"`         // Generation-derived cooperative control endpoint
	HealthSchema         int                        `json:"health_schema,omitempty"`          // Read-only notification-loop probe protocol
	OwnerSchema          int                        `json:"owner_schema,omitempty"`           // Non-zero only for an authoritative owner-bound lock
	Owner                *Owner                     `json:"owner,omitempty"`                  // Exact owner identity for an authoritative owner-bound lock
	ResumeSchema         int                        `json:"resume_schema,omitempty"`          // Agent-safe self-exec protocol; independent of OwnerSchema
	ResumeOwner          *Owner                     `json:"resume_owner,omitempty"`           // Exact coop owner used only for reload authentication/lifetime
	ResumeSignal         string                     `json:"resume_signal,omitempty"`          // Same-process restart request signal advertised by resumable wakes
	RunningImageEvidence *selfupgrade.ImageEvidence `json:"running_image_evidence,omitempty"` // Serialized running-image authority
}

const (
	FileName      = ".wake.lock"
	OwnerSchema   = 1
	OwnerWakeMode = "owner-inject-via-v1"
	OwnerFileMode = os.FileMode(0o400)

	// InjectViaMode is the wake mode of a lock bound to a .wake.target.
	InjectViaMode = "inject-via"
	// ResumeSchemaV2 marks a lock whose ResumeOwner is the coop owner.
	ResumeSchemaV2 = 2
)

// Status classifies one lock observation.
type Status string

const (
	Missing    Status = "missing"
	Valid      Status = "valid"
	Stale      Status = "stale"
	Creating   Status = "creating"
	Unverified Status = "unverified"
)

// FileReader returns a lock file's bytes and the identity of the file read.
type FileReader func() ([]byte, os.FileInfo, error)

// Observation is the result of reading one lock file without inspecting the
// process it names. Status is Missing for a decoded lock as well as for an
// absent file; Exists tells the two apart. Lock holds the decoded record
// only when the decode succeeded.
type Observation struct {
	Exists         bool
	Status         Status
	Reason         string
	Root           string
	Lock           Lock
	Raw            []byte
	FileInfo       os.FileInfo
	ObservationErr error
	DecodeErr      error
}

// Observe reads the lock through read and applies the JSON trust checks. It
// does not validate the lock format; callers apply ValidateFormat.
func Observe(root string, read FileReader) Observation {
	observation := Observation{Status: Missing}
	canonicalRoot, err := CanonicalizeRoot(root)
	if err != nil {
		observation.Status = Unverified
		observation.Reason = fmt.Sprintf("canonicalize root: %v", err)
		observation.ObservationErr = err
		return observation
	}
	observation.Root = canonicalRoot

	data, fileInfo, err := read()
	if err != nil {
		if os.IsNotExist(err) {
			return observation
		}
		observation.Exists = true
		observation.Status = Unverified
		observation.Reason = fmt.Sprintf("cannot read lock: %v", err)
		observation.ObservationErr = err
		return observation
	}

	observation.Exists = true
	observation.Raw = data
	observation.FileInfo = fileInfo
	if json.Valid(data) {
		if err := validateStateBindingFieldsJSON(data); err != nil {
			observation.Status = Unverified
			observation.Reason = err.Error()
			return observation
		}
	}
	var existing Lock
	if err := json.Unmarshal(data, &existing); err != nil {
		if json.Valid(data) {
			observation.Status = Unverified
			observation.Reason = fmt.Sprintf("wake lock JSON fields are malformed: %v", err)
			observation.DecodeErr = err
			return observation
		}
		if fileInfo != nil && fileInfo.Mode().Perm() == OwnerFileMode {
			observation.Status = Unverified
			observation.Reason = "wake owner schema is malformed; owner-bound lock may be from a newer amq"
			return observation
		}
		if fileInfo != nil && time.Since(fileInfo.ModTime()) < 2*time.Second {
			observation.Status = Creating
			observation.Reason = "lock is being created"
			return observation
		}
		observation.Status = Unverified
		observation.Reason = fmt.Sprintf("wake lock JSON is malformed: %v", err)
		return observation
	}

	observation.Lock = existing
	if err := ValidateStateBindingJSON(data); err != nil {
		observation.Status = Unverified
		observation.Reason = err.Error()
		return observation
	}
	return observation
}

// ReadFile reads the lock at path and binds the bytes to one file identity:
// the opened file must be the file that was validated before the open.
func ReadFile(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateFile(path, info); err != nil {
		return nil, nil, err
	}
	file, err := OpenReadOnly(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat wake lock: %w", err)
	}
	if err := ValidateFile(path, openedInfo); err != nil {
		return nil, nil, err
	}
	if !os.SameFile(info, openedInfo) {
		return nil, nil, NewSnapshotReadChangedError(
			fmt.Errorf("wake lock %s changed while opening", path),
		)
	}
	data, err := ReadBounded(file, "wake lock", path)
	return data, openedInfo, err
}

// ValidateFile accepts only a regular, non-symlink lock file with mode 0600 or
// 0400 that the current user owns and that group and others cannot write.
func ValidateFile(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wake lock %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake lock %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 && got != OwnerFileMode {
		return fmt.Errorf("wake lock %s mode is %o, want 0600 or 0400", path, got)
	}
	return ValidatePathOwnership("wake lock", path, info)
}

// ValidateFormat checks the lock envelope against its file mode: mode 0400 is
// an authoritative owner-bound lock, mode 0600 carries no owner markers.
func ValidateFormat(lock Lock, info os.FileInfo) error {
	if info == nil {
		return fmt.Errorf("wake lock file identity unavailable")
	}
	if err := ValidateStateBinding(lock); err != nil {
		return err
	}
	switch info.Mode().Perm() {
	case OwnerFileMode:
		if lock.OwnerSchema != OwnerSchema {
			return fmt.Errorf("wake owner schema %d unsupported; owner-bound lock may be from a newer amq", lock.OwnerSchema)
		}
		if lock.WakeMode != OwnerWakeMode {
			return fmt.Errorf("wake owner mode %q unsupported; owner-bound lock may be from a newer amq", lock.WakeMode)
		}
		if lock.Owner == nil {
			return fmt.Errorf("wake owner identity missing")
		}
		if err := ValidateAuthoritativeOwner(*lock.Owner); err != nil {
			return fmt.Errorf("wake owner identity invalid: %w", err)
		}
		if err := ValidateAuthoritativeProcessIdentity(lock); err != nil {
			return err
		}
		if strings.TrimSpace(lock.TargetDigest) == "" {
			return fmt.Errorf("wake owner target digest missing")
		}
		if strings.TrimSpace(lock.Generation) == "" {
			return fmt.Errorf("wake owner generation missing")
		}
	case 0o600:
		if lock.OwnerSchema != 0 || lock.Owner != nil || lock.WakeMode == OwnerWakeMode {
			return fmt.Errorf("wake owner markers require mode 0400")
		}
	default:
		return fmt.Errorf("wake lock mode %o unsupported", info.Mode().Perm())
	}
	return nil
}

// ValidateStateBinding checks that a state binding, when present, names this
// lock generation and target digest.
func ValidateStateBinding(lock Lock) error {
	hasGeneration := lock.StateGeneration != ""
	hasDigest := lock.StateDigest != ""
	if hasGeneration != hasDigest {
		return fmt.Errorf("wake state_generation and state_digest must be present together")
	}
	if !hasGeneration {
		return nil
	}
	if !ValidStateGeneration(lock.StateGeneration) {
		return fmt.Errorf("wake state generation is invalid")
	}
	if !ValidStateDigest(lock.StateDigest) {
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
	if lock.WakeMode != InjectViaMode && lock.WakeMode != OwnerWakeMode {
		return fmt.Errorf("wake state binding requires a target-bearing lock")
	}
	return nil
}

// Wake-lock input trust is enforced jointly by the reader's syntax gate, the
// occurrence-preserving scanner below, json.Unmarshal's known-field type
// checks, and the envelope validators. They together refuse: (1) raw parse failure; (2) non-object top
// levels; (3) known-field wrong types at early and late byte positions;
// (4) direct and folded known-field nulls; (5) top-level duplicate known keys
// in both orders and through folded aliases; (6) nested colliding keys, which
// are opaque; (7) single, duplicate, and null unknown fields, which remain
// additive ABI; and (8) valid bound and unbound controls.
func ValidateStateBindingJSON(data []byte) error {
	fields, err := decodeJSONFields(data)
	if err != nil {
		return err
	}
	if err := validateStateBindingFields(fields); err != nil {
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

func validateStateBindingFieldsJSON(data []byte) error {
	fields, err := decodeJSONFields(data)
	if err != nil {
		return err
	}
	return validateStateBindingFields(fields)
}

// decodeJSONFields preserves top-level member occurrences before map
// or struct decoding can collapse them. Only top-level known names are
// uniqueness constrained; values and unknown fields remain opaque here.
func decodeJSONFields(data []byte) (map[string]json.RawMessage, error) {
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
	knownOrder := make([]string, 0, len(knownJSONFields))
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

		canonicalName, known := jsonFieldCanonicalName(name)
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

func validateStateBindingFields(fields map[string]json.RawMessage) error {
	generation, hasGeneration := fields["state_generation"]
	digest, hasDigest := fields["state_digest"]
	if hasGeneration != hasDigest {
		return fmt.Errorf("wake state_generation and state_digest must be present together")
	}
	if hasGeneration {
		if _, err := decodeStateBindingString("state_generation", generation); err != nil {
			return err
		}
		if _, err := decodeStateBindingString("state_digest", digest); err != nil {
			return err
		}
	}
	return nil
}

var knownJSONFields = func() []string {
	typ := reflect.TypeFor[Lock]()
	fields := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		name := strings.Split(typ.Field(index).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields = append(fields, name)
		}
	}
	return fields
}()

func jsonFieldCanonicalName(name string) (string, bool) {
	for _, known := range knownJSONFields {
		if strings.EqualFold(name, known) {
			return known, true
		}
	}
	return "", false
}

func decodeStateBindingString(name string, raw json.RawMessage) (string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("wake %s must be a non-empty string", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", fmt.Errorf("wake %s must be a non-empty string", name)
	}
	return value, nil
}

// ValidateAuthoritativeProcessIdentity checks the canonical process start and
// boot id that an owner-bound lock records for the wake itself.
func ValidateAuthoritativeProcessIdentity(lock Lock) error {
	if err := validateAuthoritativeOwnerToken("wake process start", lock.ProcessStart); err != nil {
		return err
	}
	if err := validateAuthoritativeOwnerToken("wake boot id", lock.BootID); err != nil {
		return err
	}
	if !validOwnerProcessStart(lock.ProcessStart) {
		return fmt.Errorf("wake process start has malformed platform value")
	}
	if !validOwnerBootID(lock.BootID) {
		return fmt.Errorf("wake boot id has malformed platform value")
	}
	return nil
}

// CanonicalizeRoot resolves root to an absolute path without symlinks. An
// uncertain resolution is an error, never the lexical path.
func CanonicalizeRoot(root string) (string, error) {
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
