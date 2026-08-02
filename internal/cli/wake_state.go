package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// In this file, "state document" means the .wake.state artifact. Existing
// wake-check reason codes use "wake state" for notifier status vocabulary.

const (
	wakeStateSchema         = 1
	wakeStateSectionSchema  = 1
	wakeStatePreparedSchema = 1
	wakeStateFileName       = ".wake.state"
)

type wakeState struct {
	Schema   int                `json:"schema"`
	Target   wakeStateTarget    `json:"target"`
	Prepared *wakeStatePrepared `json:"prepared"`
}

type wakeStateTarget struct {
	Schema        int        `json:"schema"`
	Mode          string     `json:"mode"`
	Root          string     `json:"root"`
	Agent         string     `json:"agent"`
	Created       string     `json:"created"`
	InjectVia     string     `json:"inject_via"`
	InjectArgs    []string   `json:"inject_args,omitempty"`
	Owner         *wakeOwner `json:"owner,omitempty"`
	LegacyPresent bool       `json:"legacy_present"`
	TargetDigest  string     `json:"target_digest"`
	LegacyDigest  string     `json:"legacy_digest"`
}

type wakeStatePrepared struct {
	Schema        int    `json:"schema"`
	Generation    string `json:"generation"`
	LegacyPresent bool   `json:"legacy_present"`
	TargetDigest  string `json:"target_digest"`
	LegacyDigest  string `json:"legacy_digest"`
}

type wakeStateLegacyPrepared struct {
	Schema       int
	Generation   string
	TargetDigest string
}

type wakeStateLegacy struct {
	Target      *wakeTarget
	TargetRaw   []byte
	Prepared    *wakeStateLegacyPrepared
	PreparedRaw []byte
}

type wakeStateLegacyMismatchError struct {
	reason string
}

func (err *wakeStateLegacyMismatchError) Error() string {
	if err == nil {
		return "wake state legacy mismatch"
	}
	return "wake state legacy mismatch: " + err.reason
}

type wakeStatePreparedObservation string

const (
	wakeStatePreparedAbsent  wakeStatePreparedObservation = "absent"
	wakeStatePreparedStale   wakeStatePreparedObservation = "stale"
	wakeStatePreparedCurrent wakeStatePreparedObservation = "current"
	wakeStatePreparedRefused wakeStatePreparedObservation = "refused"
)

func newWakeState(legacy wakeStateLegacy) (wakeState, error) {
	if legacy.Target == nil {
		return wakeState{}, fmt.Errorf("wake state target is missing")
	}
	if len(legacy.TargetRaw) == 0 {
		return wakeState{}, fmt.Errorf("wake state target legacy bytes are missing")
	}
	targetDigest, err := wakeTargetDigest(*legacy.Target)
	if err != nil {
		return wakeState{}, err
	}
	state := wakeState{
		Schema: wakeStateSchema,
		Target: wakeStateTarget{
			Schema:        legacy.Target.Schema,
			Mode:          legacy.Target.Mode,
			Root:          legacy.Target.Root,
			Agent:         legacy.Target.Agent,
			Created:       legacy.Target.Created,
			InjectVia:     legacy.Target.InjectVia,
			InjectArgs:    append([]string(nil), legacy.Target.InjectArgs...),
			Owner:         cloneWakeStateOwner(legacy.Target.Owner),
			LegacyPresent: true,
			TargetDigest:  targetDigest,
			LegacyDigest:  wakeLegacyDigest(legacy.TargetRaw),
		},
	}
	if legacy.Prepared != nil {
		if len(legacy.PreparedRaw) == 0 {
			return wakeState{}, fmt.Errorf("wake state prepared legacy bytes are missing")
		}
		state.Prepared = &wakeStatePrepared{
			Schema:        legacy.Prepared.Schema,
			Generation:    legacy.Prepared.Generation,
			LegacyPresent: true,
			TargetDigest:  legacy.Prepared.TargetDigest,
			LegacyDigest:  wakeLegacyDigest(legacy.PreparedRaw),
		}
	} else if len(legacy.PreparedRaw) != 0 {
		return wakeState{}, fmt.Errorf("wake state prepared existence is inconsistent")
	}
	if err := validateWakeState(state); err != nil {
		return wakeState{}, err
	}
	return state, nil
}

func encodeWakeState(state wakeState) ([]byte, error) {
	if err := validateWakeState(state); err != nil {
		return nil, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal wake state: %w", err)
	}
	return data, nil
}

func decodeWakeState(data []byte) (wakeState, error) {
	if len(data) == 0 || len(data) > maxWakeMetadataFileBytes {
		return wakeState{}, fmt.Errorf("wake state size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state wakeState
	if err := decoder.Decode(&state); err != nil {
		return wakeState{}, fmt.Errorf("decode wake state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return wakeState{}, fmt.Errorf("decode wake state: trailing JSON value")
		}
		return wakeState{}, fmt.Errorf("decode wake state trailing bytes: %w", err)
	}
	if err := validateWakeState(state); err != nil {
		return wakeState{}, err
	}
	canonical, err := json.Marshal(state)
	if err != nil {
		return wakeState{}, fmt.Errorf("re-encode wake state: %w", err)
	}
	if !bytes.Equal(canonical, data) {
		return wakeState{}, fmt.Errorf("wake state is not canonical")
	}
	return state, nil
}

func validateWakeState(state wakeState) error {
	if state.Schema != wakeStateSchema {
		return fmt.Errorf("wake state schema %d unsupported", state.Schema)
	}
	if err := validateWakeStateTarget(state.Target); err != nil {
		return err
	}
	if state.Prepared != nil {
		if err := validateWakeStatePrepared(*state.Prepared); err != nil {
			return err
		}
	}
	return nil
}

func validateWakeStateTarget(target wakeStateTarget) error {
	if target.Schema != wakeStateSectionSchema {
		return fmt.Errorf("wake state target schema %d unsupported", target.Schema)
	}
	if target.Mode != wakeTargetInjectVia {
		return fmt.Errorf("wake state target mode %q unsupported", target.Mode)
	}
	if target.Root == "" || target.Root != strings.TrimSpace(target.Root) ||
		strings.ContainsRune(target.Root, 0) || !filepath.IsAbs(target.Root) ||
		filepath.Clean(target.Root) != target.Root || canonicalWakeRoot(target.Root) != target.Root {
		return fmt.Errorf("wake state target root is invalid")
	}
	if err := fsq.ValidateHandle(target.Agent); err != nil {
		return fmt.Errorf("wake state target agent is invalid: %w", err)
	}
	created, err := time.Parse(time.RFC3339, target.Created)
	if err != nil || created.UTC().Format(time.RFC3339) != target.Created {
		return fmt.Errorf("wake state target created timestamp is invalid")
	}
	if target.InjectVia == "" || target.InjectVia != strings.TrimSpace(target.InjectVia) ||
		strings.ContainsRune(target.InjectVia, 0) || !filepath.IsAbs(target.InjectVia) ||
		filepath.Clean(target.InjectVia) != target.InjectVia {
		return fmt.Errorf("wake state target inject_via is invalid")
	}
	for _, arg := range target.InjectArgs {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("wake state target inject arg contains NUL")
		}
	}
	if target.Owner != nil {
		if err := validateAuthoritativeWakeOwner(*target.Owner); err != nil {
			return fmt.Errorf("wake state target owner is invalid: %w", err)
		}
	}
	if !target.LegacyPresent {
		return fmt.Errorf("wake state target legacy presence is false")
	}
	if !validWakeStateDigest(target.TargetDigest) {
		return fmt.Errorf("wake state target semantic digest is invalid")
	}
	if !validWakeStateDigest(target.LegacyDigest) {
		return fmt.Errorf("wake state target legacy digest is invalid")
	}
	semantic, err := wakeTargetDigest(target.wakeTarget())
	if err != nil {
		return err
	}
	if semantic != target.TargetDigest {
		return fmt.Errorf("wake state target semantic digest mismatch")
	}
	return nil
}

func validateWakeStatePrepared(prepared wakeStatePrepared) error {
	if prepared.Schema != wakeStatePreparedSchema {
		return fmt.Errorf("wake state prepared schema %d unsupported", prepared.Schema)
	}
	if !validWakeStateGeneration(prepared.Generation) {
		return fmt.Errorf("wake state prepared generation is invalid")
	}
	if !prepared.LegacyPresent {
		return fmt.Errorf("wake state prepared legacy presence is false")
	}
	if !validWakeStateDigest(prepared.TargetDigest) {
		return fmt.Errorf("wake state prepared target digest is invalid")
	}
	if !validWakeStateDigest(prepared.LegacyDigest) {
		return fmt.Errorf("wake state prepared legacy digest is invalid")
	}
	return nil
}

func validateWakeStateAgainstLegacy(state wakeState, legacy wakeStateLegacy) error {
	if err := validateWakeState(state); err != nil {
		return err
	}
	if legacy.Target == nil {
		return newWakeStateLegacyMismatch("target exists in state but not legacy")
	}
	if len(legacy.TargetRaw) == 0 {
		return newWakeStateLegacyMismatch("target legacy bytes are missing")
	}
	wantTarget, err := wakeTargetDigest(*legacy.Target)
	if err != nil {
		return err
	}
	if state.Target.TargetDigest != wantTarget {
		return newWakeStateLegacyMismatch("target semantic digest mismatch")
	}
	if state.Target.LegacyDigest != wakeLegacyDigest(legacy.TargetRaw) {
		return newWakeStateLegacyMismatch("target legacy digest mismatch")
	}
	if (state.Prepared == nil) != (legacy.Prepared == nil) {
		return newWakeStateLegacyMismatch("prepared existence mismatch")
	}
	if state.Prepared == nil {
		if len(legacy.PreparedRaw) != 0 {
			return newWakeStateLegacyMismatch("prepared raw existence mismatch")
		}
		return nil
	}
	if len(legacy.PreparedRaw) == 0 {
		return newWakeStateLegacyMismatch("prepared legacy bytes are missing")
	}
	if state.Prepared.Schema != legacy.Prepared.Schema ||
		state.Prepared.Generation != legacy.Prepared.Generation ||
		state.Prepared.TargetDigest != legacy.Prepared.TargetDigest {
		return newWakeStateLegacyMismatch("prepared semantics mismatch")
	}
	if state.Prepared.LegacyDigest != wakeLegacyDigest(legacy.PreparedRaw) {
		return newWakeStateLegacyMismatch("prepared legacy digest mismatch")
	}
	return nil
}

func classifyWakeStatePrepared(
	prepared *wakeStatePrepared,
	currentGeneration string,
	currentTargetDigest string,
) wakeStatePreparedObservation {
	if prepared == nil {
		return wakeStatePreparedAbsent
	}
	if prepared.Generation != currentGeneration {
		return wakeStatePreparedStale
	}
	if prepared.TargetDigest != currentTargetDigest {
		return wakeStatePreparedRefused
	}
	return wakeStatePreparedCurrent
}

func wakeLegacyDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func wakeCanonicalStateDigest(data []byte) (string, error) {
	if _, err := decodeWakeState(data); err != nil {
		return "", err
	}
	return wakeLegacyDigest(data), nil
}

func validWakeStateDigest(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return false
	}
	encoded := value[len(prefix):]
	if encoded != strings.ToLower(encoded) {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size
}

func validWakeStateGeneration(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}

func (target wakeStateTarget) wakeTarget() wakeTarget {
	return wakeTarget{
		Schema:     target.Schema,
		Mode:       target.Mode,
		Root:       target.Root,
		Agent:      target.Agent,
		Created:    target.Created,
		InjectVia:  target.InjectVia,
		InjectArgs: append([]string(nil), target.InjectArgs...),
		Owner:      cloneWakeStateOwner(target.Owner),
	}
}

func cloneWakeStateOwner(owner *wakeOwner) *wakeOwner {
	if owner == nil {
		return nil
	}
	cloned := *owner
	return &cloned
}

func newWakeStateLegacyMismatch(reason string) error {
	return &wakeStateLegacyMismatchError{reason: reason}
}
