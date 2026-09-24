// Package binding stores which live session the per-user Buzz agent drives.
// `amq-remote attach --self` writes it at the moment the owner consents
// inside that session; amq-acp in binding mode reads it on every prompt.
// A request keeps the binding it was submitted under, so a later attach
// never moves work that is already running.
package binding

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// EnvPath overrides the binding file location (tests, alternate homes).
const EnvPath = "AMQ_REMOTE_BINDING"

// Binding names one shared native session.
type Binding struct {
	// Root is the AMQ root whose amq-remote endpoint owns Target.
	Root string `json:"root"`
	// Target is the endpoint target id, for example claude:12345.
	Target string `json:"target"`
	// NativeSession is the native identity the owner shared. The endpoint
	// refuses every command once the target is attached to another one.
	NativeSession string `json:"native_session"`
	// Display is human text for replies; never an identity.
	Display string `json:"display,omitempty"`
	BoundAt string `json:"bound_at"`
}

// ErrNone is returned when no session is bound.
var ErrNone = errors.New("no session is bound; run /amq-remote in a session")

// Path is the binding file: $AMQ_REMOTE_BINDING, or ~/.amq/remote/binding.json.
func Path() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvPath)); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be absolute", EnvPath)
		}
		return filepath.Clean(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".amq", "remote", "binding.json"), nil
}

// Read returns the current binding, or ErrNone.
func Read() (Binding, error) {
	path, err := Path()
	if err != nil {
		return Binding{}, err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Binding{}, ErrNone
	}
	if err != nil {
		return Binding{}, err
	}
	if !fi.Mode().IsRegular() {
		return Binding{}, fmt.Errorf("binding %s is not a regular file; refusing", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Binding{}, err
	}
	var b Binding
	if err := json.Unmarshal(raw, &b); err != nil {
		return Binding{}, fmt.Errorf("binding %s: %w", path, err)
	}
	if !filepath.IsAbs(b.Root) || b.Target == "" || b.NativeSession == "" {
		return Binding{}, fmt.Errorf("binding %s is incomplete; run /amq-remote again", path)
	}
	return b, nil
}

// Write replaces the binding atomically with mode 0600.
func Write(b Binding) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if b.BoundAt == "" {
		b.BoundAt = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("binding %s is not a regular file; refusing", path)
	}
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), append(raw, '\n'), 0o600)
	return err
}

// Remove deletes the binding. With target set, it removes the binding only
// when it names that target, so one session's off never unbinds another.
func Remove(target string) (bool, error) {
	b, err := Read()
	if errors.Is(err, ErrNone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if target != "" && b.Target != target {
		return false, nil
	}
	path, err := Path()
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}
