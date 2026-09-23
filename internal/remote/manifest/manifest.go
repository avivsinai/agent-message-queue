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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
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
	// Relay is the optional Buzz relay surface (bead 611.15). Absent means
	// no relay network traffic. It requires schema_version 2, so an older
	// binary refuses the file instead of silently ignoring the object.
	Relay *Relay `json:"relay,omitempty"`
}

// RelaySchemaVersion is the manifest version that may carry a relay object.
const RelaySchemaVersion = 2

// Relay configures one relay endpoint and the sessions shared on it.
type Relay struct {
	// URL is wss://, or ws:// to a loopback host for in-process tests.
	URL string `json:"url"`
	// Self optionally pins the relay's NIP-11 self key, the key that signs
	// NIP-29 group membership. Unset, it is read from the relay's NIP-11
	// document on each connection.
	Self   string  `json:"relay_self,omitempty"`
	Shares []Share `json:"shares"`
}

// Share binds one enrolled body (keys/<session>) to exactly one declared
// adapter target. Surfaces beyond authentication (commands, activity) are
// refused until their slices ship, so the file can never claim a surface
// the binary does not have.
type Share struct {
	Target      string `json:"target"`
	Session     string `json:"session"`
	OwnerPubKey string `json:"owner_pubkey"`
	// DMChannelID is the owner's one-to-one private Buzz channel, bound
	// explicitly by the operator. Commands require it.
	DMChannelID string `json:"dm_channel_id,omitempty"`
	// NativeSessionID pins the harness session the operator approved for
	// sharing (the target's inspected native_session_id). Commands require
	// it; a different session under the same target is never shared by
	// inheritance, and serve never rewrites it.
	NativeSessionID string `json:"native_session_id,omitempty"`
	// MentionChannels are channels where an owner message that mentions the
	// body submits a request; its output goes to the DM channel, never to
	// the mentioning channel. Commands require dm_channel_id first.
	MentionChannels []string `json:"mention_channels,omitempty"`
	Commands        bool     `json:"commands,omitempty"`
	// Presence publishes the body's kind 0 profile and kind 10100 status so
	// the owner's Buzz Desktop lists it. Name is the display name, published
	// in clear text; it must not carry a path or prompt data.
	Presence bool   `json:"presence,omitempty"`
	Name     string `json:"name,omitempty"`
	Activity bool   `json:"activity,omitempty"`
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
	if f.SchemaVersion != SchemaVersion && f.SchemaVersion != RelaySchemaVersion {
		return File{}, fmt.Errorf("manifest schema_version %d, want %d or %d", f.SchemaVersion, SchemaVersion, RelaySchemaVersion)
	}
	// Layer defaults to "remote" in memory when absent. A non-empty layer
	// other than "remote" is a validation error (caught by Validate).
	if f.Layer == "" {
		f.Layer = Layer
	}
	if f.Relay != nil {
		if err := strictRelay(data); err != nil {
			return File{}, err
		}
	}
	if err := Validate(f); err != nil {
		return File{}, err
	}
	return f, nil
}

// strictRelay re-decodes the relay object refusing unknown fields: a typo
// in a v2 relay or share field must be a usage error, never a silently
// ignored setting (codex slice 1 review #8).
func strictRelay(data []byte) error {
	var top struct {
		Relay json.RawMessage `json:"relay"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(top.Relay))
	dec.DisallowUnknownFields()
	var r Relay
	if err := dec.Decode(&r); err != nil {
		return &ErrInvalidRelay{Reason: err.Error()}
	}
	return nil
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

// ErrWrongLayer is returned when the manifest's layer field is present but
// not "remote". A manifest without a layer field is valid (defaults to
// "remote" in memory); a layer field with any other value is a validation
// error.
type ErrWrongLayer struct {
	Layer string
}

func (e *ErrWrongLayer) Error() string {
	return "manifest: layer field must be \"remote\" or absent, got " + strconv.Quote(e.Layer)
}

// ErrMissingField is returned when an adapter entry lacks its target or kind.
type ErrMissingField struct {
	Field   string
	Context string
}

func (e *ErrMissingField) Error() string { return e.Field + " is required (" + e.Context + ")" }

// ErrInvalidTarget is returned when a manifest target id is not a
// protocol-valid opaque id. An entry Validate accepts must be addressable:
// the endpoint registers it and every CLI submit is checked against the same
// rule (protocol.validOpaque). Accepting "sales team" here produced a
// session no client could reach (611.13 r4).
type ErrInvalidTarget struct {
	Target string
}

func (e *ErrInvalidTarget) Error() string {
	return "manifest: target " + strconv.Quote(e.Target) + " is not a valid target id (must be non-empty, at most " + strconv.Itoa(protocol.MaxOpaqueLen) + " bytes, characters [A-Za-z0-9_.:-])"
}

// ErrInvalidEpoch is returned when a fake adapter's test-only epoch is not a
// protocol-valid opaque epoch. The fake requires an epoch at submit time; an
// accepted empty-epoch entry produced an unusable session (611.13 r4).
type ErrInvalidEpoch struct {
	Target string
	Epoch  string
}

func (e *ErrInvalidEpoch) Error() string {
	return "manifest: fake adapter " + strconv.Quote(e.Target) + " epoch " + strconv.Quote(e.Epoch) + " is not a valid epoch (must be non-empty, at most " + strconv.Itoa(protocol.MaxOpaqueLen) + " bytes, characters [A-Za-z0-9_.:-])"
}

// ValidationError is implemented by every manifest validation error so
// IsValidation can use a single errors.As check (the errors.Is version was
// always false against typed-nil pointers).
type ValidationError interface {
	error
	validationError()
}

func (*ErrDuplicateTarget) validationError() {}
func (*ErrEpochOnNonFake) validationError()  {}
func (*ErrWrongLayer) validationError()      {}
func (*ErrMissingField) validationError()    {}
func (*ErrInvalidTarget) validationError()   {}
func (*ErrInvalidEpoch) validationError()    {}

// IsValidation reports whether err is a manifest validation failure as
// opposed to a filesystem or parse failure. Validation failures are usage
// errors (exit 2); I/O and parse failures are not.
func IsValidation(err error) bool {
	var ve ValidationError
	return errors.As(err, &ve)
}

// Validate checks the manifest: layer defaults to "remote" when absent (a
// user-authored manifest without layer is valid); a non-empty layer other
// than "remote" is a validation error. No duplicate targets; target and kind
// required; epoch only on fake.
// Validate checks the manifest: layer defaults to "remote" when absent (a
// user-authored manifest without layer is valid); a non-empty layer other
// than "remote" is a validation error. No duplicate targets; target and kind
// required and protocol-valid (opaque grammar); fake requires a valid epoch;
// epoch only on fake.
func Validate(f File) error {
	if f.Layer != "" && f.Layer != Layer {
		return &ErrWrongLayer{Layer: f.Layer}
	}
	seen := make(map[string]bool, len(f.Adapters))
	for _, a := range f.Adapters {
		if a.Target == "" {
			return &ErrMissingField{Field: "target", Context: "adapter kind " + strconv.Quote(a.Kind)}
		}
		if !protocol.ValidTargetID(a.Target) {
			return &ErrInvalidTarget{Target: a.Target}
		}
		if seen[a.Target] {
			return &ErrDuplicateTarget{Target: a.Target}
		}
		seen[a.Target] = true
		if a.Kind == "" {
			return &ErrMissingField{Field: "kind", Context: "adapter target " + strconv.Quote(a.Target)}
		}
		if a.Kind == "fake" {
			if !protocol.ValidEpoch(a.Epoch) {
				return &ErrInvalidEpoch{Target: a.Target, Epoch: a.Epoch}
			}
		} else if a.Epoch != "" {
			return &ErrEpochOnNonFake{Target: a.Target, Kind: a.Kind}
		}
	}
	return validateRelay(f, seen)
}

// ErrInvalidRelay is a relay object that cannot be used as written.
type ErrInvalidRelay struct{ Reason string }

func (e *ErrInvalidRelay) Error() string { return "manifest relay: " + e.Reason }

// ErrInvalidRelay is a usage error like the other validation failures.
func (*ErrInvalidRelay) validationError() {}

func validateRelay(f File, targets map[string]bool) error {
	r := f.Relay
	if r == nil {
		return nil
	}
	if f.SchemaVersion != RelaySchemaVersion {
		return &ErrInvalidRelay{Reason: fmt.Sprintf("a relay object needs schema_version %d", RelaySchemaVersion)}
	}
	if err := validRelayURL(r.URL); err != nil {
		return &ErrInvalidRelay{Reason: err.Error()}
	}
	if r.Self != "" && !validHex64(r.Self) {
		return &ErrInvalidRelay{Reason: "relay_self must be 64 lowercase hex"}
	}
	if len(r.Shares) == 0 {
		return &ErrInvalidRelay{Reason: "shares is empty"}
	}
	sessions, bound := map[string]bool{}, map[string]bool{}
	for _, sh := range r.Shares {
		switch {
		case !targets[sh.Target]:
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share target %q is not a declared adapter", sh.Target)}
		case bound[sh.Target]:
			return &ErrInvalidRelay{Reason: fmt.Sprintf("target %q is shared twice", sh.Target)}
		case !validSession(sh.Session):
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share session %q must be one path component", sh.Session)}
		case sessions[sh.Session]:
			return &ErrInvalidRelay{Reason: fmt.Sprintf("session %q is shared twice", sh.Session)}
		case !validHex64(sh.OwnerPubKey):
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share %q owner_pubkey must be 64 lowercase hex", sh.Session)}
		case sh.Commands && sh.DMChannelID == "":
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share %q: commands need dm_channel_id, the owner's private DM channel", sh.Session)}
		case sh.Commands && sh.NativeSessionID == "":
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share %q: commands need native_session_id, the approved native session (see amq-remote inspect)", sh.Session)}
		case sh.Activity && sh.NativeSessionID == "":
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share %q: activity needs native_session_id, the approved native session", sh.Session)}
		case sh.Presence && !validDisplayName(sh.Name):
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share %q: presence needs a name of 1 to 64 printable characters with no path separator", sh.Session)}
		case len(sh.MentionChannels) > 0 && !sh.Commands:
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share %q: mention_channels need commands", sh.Session)}
		case len(sh.MentionChannels) > maxMentionChannels:
			return &ErrInvalidRelay{Reason: fmt.Sprintf("share %q: at most %d mention_channels", sh.Session, maxMentionChannels)}
		}
		seen := map[string]bool{sh.DMChannelID: true}
		for _, ch := range sh.MentionChannels {
			if ch == "" || seen[ch] {
				return &ErrInvalidRelay{Reason: fmt.Sprintf("share %q: mention channel %q is empty, repeated, or the DM channel", sh.Session, ch)}
			}
			seen[ch] = true
		}
		bound[sh.Target], sessions[sh.Session] = true, true
	}
	return nil
}

// validDisplayName is a short printable name with no path separator, so a
// clear-text profile cannot leak a local path.
func validDisplayName(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > 64 || strings.ContainsAny(s, "/\\") {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// maxMentionChannels bounds one share's mention subscription filter.
const maxMentionChannels = 16

func validSession(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

func validHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validRelayURL mirrors internal/relay.ValidateURL (kept here so the
// manifest package stays dependency-free): wss://, or ws:// to loopback,
// never userinfo.
func validRelayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url: %v", err)
	}
	if u.User != nil {
		return errors.New("url must not carry userinfo")
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		host := u.Hostname()
		if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
			return nil
		}
		return errors.New("ws:// is allowed only to a loopback host; use wss://")
	default:
		return fmt.Errorf("url scheme %q: want wss://", u.Scheme)
	}
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
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("manifest %s is a symlink; refusing", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("manifest %s: %w", path, err)
	}
	if _, err := fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), append(data, '\n'), 0644); err != nil {
		return fmt.Errorf("write manifest %s: %w", path, err)
	}
	return nil
}
