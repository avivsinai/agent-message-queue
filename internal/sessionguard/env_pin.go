package sessionguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Environment variables that carry an inherited AMQ session pin. They are the
// same ones amq itself sets.
const (
	EnvRoot       = "AM_ROOT"
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

// VerifyEnvPin authenticates the inherited AMQ session pin (AM_SESSION,
// AM_BASE_ROOT and the identity tokens) against root, for companions that bind
// a mailbox from the environment, so a foreign AM_ROOT is refused. The amq CLI
// applies its own pin policy through Decide. Pin evidence without an exact
// base root, a root that disagrees with the pinned base and session, and
// identity tokens that no longer name the same physical directories are all
// refusals.
func VerifyEnvPin(root string) error { return verifyEnvPinWith(root, "") }

// VerifyEnvPinOn authenticates the pin against an already-open root
// capability: the root's identity token is compared with the capability's own
// directory, not a fresh lookup of the path, so the caller then writes into
// exactly the directory that was authenticated.
func VerifyEnvPinOn(root *fsq.DeliveryRoot) error {
	info := root.FileInfo()
	if info == nil {
		return contextError("no root identity to authenticate")
	}
	tree, err := fsq.StableTreeIdentityInfo(info)
	if err != nil {
		return contextError("cannot authenticate %s: %v", EnvRootID, err)
	}
	return verifyEnvPinWith(root.Base(), tree)
}

// verifyEnvPinWith is VerifyEnvPin with the root's live identity token
// supplied by a capability; empty means look the path up.
func verifyEnvPinWith(root, rootTree string) error {
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
	if rootTree != "" {
		if rootTree != rootID {
			return contextError("%s no longer identifies %s; refusing to deliver into a replaced tree", EnvRootID, root)
		}
	} else if err := verifyTreeIdentity(root, rootID, EnvRootID); err != nil {
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
