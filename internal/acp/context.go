// Package acp implements a preview Agent Client Protocol (ACP) version 1
// companion. It speaks ACP over stdio and turns each prompt into an ordinary
// AMQ message. It never opens a socket, never executes tools, and never
// advertises ACP version 2.
package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Environment variables that select the routing context. The pin variables are
// the same ones amq itself honors. This companion authenticates them and
// refuses on a mismatch instead of restating the CLI's routing policy.
const (
	EnvRoot       = "AM_ROOT"
	EnvMe         = "AM_ME"
	EnvTo         = "AMQ_ACP_TO"
	EnvBaseRoot   = "AM_BASE_ROOT"
	EnvSession    = "AM_SESSION"
	EnvRootID     = "AM_ROOT_ID"
	EnvBaseRootID = "AM_BASE_ROOT_ID"
)

// ContextError marks a fail-closed routing refusal so the caller can report the
// repository's context-mismatch exit code.
type ContextError struct {
	Message string
}

func (e *ContextError) Error() string {
	return e.Message
}

func contextError(format string, args ...any) error {
	return &ContextError{Message: fmt.Sprintf(format, args...)}
}

// Config is the resolved routing context for one amq-acp process.
type Config struct {
	Root string
	Me   string
	To   string
}

// LoadConfig resolves the routing context from the environment. An absent root,
// an unusable handle, or a session pin that cannot be authenticated all refuse;
// there is no fallback to a guessed root.
func LoadConfig() (Config, error) {
	root := strings.TrimSpace(os.Getenv(EnvRoot))
	if root == "" {
		return Config{}, contextError("%s is not set; amq-acp requires an explicit queue root", EnvRoot)
	}
	if !filepath.IsAbs(root) {
		return Config{}, contextError("invalid %s=%q: queue root must be absolute", EnvRoot, root)
	}
	root = filepath.Clean(root)

	me := strings.TrimSpace(os.Getenv(EnvMe))
	if err := fsq.ValidateHandle(me); err != nil {
		return Config{}, contextError("%s: %v", EnvMe, err)
	}
	to := strings.TrimSpace(os.Getenv(EnvTo))
	if err := fsq.ValidateHandle(to); err != nil {
		return Config{}, contextError("%s: %v", EnvTo, err)
	}

	if err := verifySessionPin(root); err != nil {
		return Config{}, err
	}
	return Config{Root: root, Me: me, To: to}, nil
}

// verifySessionPin authenticates an inherited pin against the target root. Pin
// evidence without an exact base root, a root that disagrees with the pinned
// base and session, and identity tokens that no longer name the same physical
// directories are all refusals.
func verifySessionPin(root string) error {
	session, sessionPresent := os.LookupEnv(EnvSession)
	rootID, rootIDPresent := os.LookupEnv(EnvRootID)
	baseRootID, baseRootIDPresent := os.LookupEnv(EnvBaseRootID)
	if !sessionPresent && !rootIDPresent && !baseRootIDPresent {
		return nil
	}

	session = strings.TrimSpace(session)
	if session != "" {
		if err := validateSessionName(session); err != nil {
			return err
		}
	}

	base := strings.TrimSpace(os.Getenv(EnvBaseRoot))
	if base == "" {
		return contextError(
			"incomplete AMQ session pin: evidence from %s, %s, or %s requires an exact %s",
			EnvSession, EnvRootID, EnvBaseRootID, EnvBaseRoot,
		)
	}
	if !filepath.IsAbs(base) {
		return contextError("invalid %s=%q: pinned base root must be absolute", EnvBaseRoot, base)
	}
	base = filepath.Clean(base)

	expected := base
	if session != "" {
		expected = filepath.Join(base, session)
	}
	if root != expected {
		return contextError(
			"session context mismatch: %s=%s differs from pinned root %s (%s=%q)",
			EnvRoot, root, expected, EnvSession, session,
		)
	}

	if !rootIDPresent && !baseRootIDPresent {
		return nil
	}
	rootID = strings.TrimSpace(rootID)
	baseRootID = strings.TrimSpace(baseRootID)
	if rootID == "" || baseRootID == "" {
		return contextError(
			"incomplete AMQ identity pin: %s and %s must both be present and non-empty",
			EnvRootID, EnvBaseRootID,
		)
	}
	if err := verifyTreeIdentity(root, rootID, EnvRootID); err != nil {
		return err
	}
	return verifyTreeIdentity(base, baseRootID, EnvBaseRootID)
}

// verifyTreeIdentity compares a pinned identity token against the live
// directory. An unreadable path is uncertain, not proof of a match.
func verifyTreeIdentity(path, token, name string) error {
	current, err := fsq.StableTreeIdentity(path)
	if err != nil {
		return contextError("cannot authenticate %s against %s: %v", name, path, err)
	}
	if current != token {
		return contextError("%s no longer identifies %s; refusing to deliver into a replaced tree", name, path)
	}
	return nil
}

// validateSessionName mirrors the session name grammar amq accepts.
func validateSessionName(name string) error {
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return contextError("invalid %s=%q (allowed: a-z, 0-9, -, _)", EnvSession, name)
	}
	return nil
}
