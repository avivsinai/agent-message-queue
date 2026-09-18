// Package manifest defines the companion adapter manifest: the single source
// of truth for which adapters a serve instance runs. The manifest lives at
// <AM_ROOT>/extensions/remote/manifest.json (per docs/adr-layer-extensions.md
// the extensions/<layer> directory is owned by the layer; the companion owns
// extensions/remote). The --manifest flag overrides the path for tests only.
//
// The manifest is intentionally passive for lifecycle: it is read at serve
// start (cold reload). A target absent from the manifest on the next start is
// not registered, and its live records reconcile as attachment_lost through
// the existing Reconcile path. Hot-reload (SIGHUP) is a later bead.
//
// Flags (--fake, --codex-socket) APPEND to the manifest set. A target id
// present in both is a usage error (exit 2). The manifest never stores an
// epoch: the epoch is OBSERVED from the attachment's Inspect at attach time
// (ADR invariant 5), never read from the manifest. The fake's "epoch" field
// is test-only; the validator rejects it for any other kind.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// SchemaVersion is the manifest format version.
const SchemaVersion = 1

// Layer is the companion's extension-layer name. The companion owns
// extensions/remote, so the manifest at extensions/remote/manifest.json is a
// passive extension manifest with layer "remote" (the ADR layer contract that
// `amq doctor` validates).
const Layer = "remote"

// File is the top-level manifest document.
type File struct {
	SchemaVersion int       `json:"schema_version"`
	Layer         string    `json:"layer"`
	Adapters      []Adapter `json:"adapters"`
}

// Adapter is one adapter instance in the manifest.
type Adapter struct {
	// Kind names the factory: "fake", "codex", "amit", "claude".
	Kind string `json:"kind"`
	// Target is the target id; it is the manifest key and must be unique.
	Target string `json:"target"`
	// Epoch is test-only for the fake adapter; rejected for any other kind.
	// The live epoch is observed from Inspect at attach time, never from here.
	Epoch string `json:"epoch,omitempty"`
	// Config is the opaque adapter-specific configuration block.
	Config json.RawMessage `json:"config,omitempty"`
}

// DefaultPath returns the manifest path inside the companion's stateDir.
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, "manifest.json")
}

// Load reads and validates the manifest at path. An empty or absent file
// yields a valid zero-adapter manifest (serve with no targets is legal).
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return File{SchemaVersion: SchemaVersion, Layer: Layer}, nil
		}
		return File{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	if f.SchemaVersion != SchemaVersion {
		return File{}, fmt.Errorf("manifest schema_version %d, want %d", f.SchemaVersion, SchemaVersion)
	}
	if err := Validate(f); err != nil {
		return File{}, err
	}
	return f, nil
}

// ErrDuplicateTarget is returned when two adapters share a target id.
type ErrDuplicateTarget struct {
	Target string
}

func (e *ErrDuplicateTarget) Error() string {
	return fmt.Sprintf("duplicate target %q in manifest", e.Target)
}

// ErrEpochOnNonFake is returned when a non-fake adapter declares an epoch.
type ErrEpochOnNonFake struct {
	Target string
	Kind   string
}

func (e *ErrEpochOnNonFake) Error() string {
	return fmt.Sprintf("adapter %q (kind %q): epoch is test-only for fake and rejected for other kinds", e.Target, e.Kind)
}

// ErrMissingLayer is returned when the passive-manifest layer field is
// absent. A remote manifest without layer cannot be diagnosed by `amq
// doctor`'s extension scan.
type ErrMissingLayer struct{}

func (e *ErrMissingLayer) Error() string {
	return "manifest: layer field is required (passive extension-manifest contract)"
}

// ErrMissingField is returned when an adapter entry lacks its target or kind.
type ErrMissingField struct {
	Field   string
	Context string
}

func (e *ErrMissingField) Error() string { return e.Field + " is required (" + e.Context + ")" }

// IsValidation reports whether err is a manifest validation failure as
// opposed to a filesystem or parse failure. Validation failures are usage
// errors (exit 2); I/O and parse failures are not.
func IsValidation(err error) bool {
	var dup *ErrDuplicateTarget
	var epoch *ErrEpochOnNonFake
	var missing *ErrMissingLayer
	var field *ErrMissingField
	return errors.Is(err, dup) || errors.Is(err, epoch) || errors.Is(err, missing) || errors.Is(err, field)
}

// Validate checks the manifest: layer present, no duplicate targets, target
// and kind required, epoch only on fake.
func Validate(f File) error {
	if f.Layer == "" {
		return &ErrMissingLayer{}
	}
	seen := make(map[string]bool, len(f.Adapters))
	for _, a := range f.Adapters {
		if a.Target == "" {
			return &ErrMissingField{Field: "target", Context: "adapter kind " + strconv.Quote(a.Kind)}
		}
		if seen[a.Target] {
			return &ErrDuplicateTarget{Target: a.Target}
		}
		seen[a.Target] = true
		if a.Kind == "" {
			return &ErrMissingField{Field: "kind", Context: "adapter target " + strconv.Quote(a.Target)}
		}
		if a.Epoch != "" && a.Kind != "fake" {
			return &ErrEpochOnNonFake{Target: a.Target, Kind: a.Kind}
		}
	}
	return nil
}

// Write persists the manifest, filling the passive-manifest layer field
// ("remote", per the extensions/remote ownership) when unset. It creates the
// parent directory. serve writes the merged (file + flag) set here so the
// manifest is the single source of truth and `amq doctor` can diagnose it.
func Write(path string, f File) error {
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	if f.Layer == "" {
		f.Layer = Layer
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		return fmt.Errorf("write manifest %s: %w", path, err)
	}
	return nil
}
