// Package acp implements the Agent Client Protocol (ACP) v2 AMQ bridge. It
// speaks ACP over stdio, keeps prompt turns open for live AMQ replies, and
// never opens a socket or executes tools.
package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Environment variables that select the routing context. The pin variables are
// the same ones amq itself honors. This companion authenticates them and
// refuses on a mismatch instead of restating the CLI's routing policy.
const (
	EnvRoot              = "AM_ROOT"
	EnvMe                = "AM_ME"
	EnvTo                = "AMQ_ACP_TO"
	EnvBaseRoot          = "AM_BASE_ROOT"
	EnvSession           = "AM_SESSION"
	EnvRootID            = "AM_ROOT_ID"
	EnvBaseRootID        = "AM_BASE_ROOT_ID"
	EnvStateDir          = "AMQ_ACP_STATE_DIR"
	EnvTurnTimeout       = "AMQ_ACP_TURN_TIMEOUT"
	EnvIdleTimeout       = "AMQ_ACP_IDLE_TIMEOUT"
	EnvPollInterval      = "AMQ_ACP_POLL_INTERVAL"
	EnvHeartbeatInterval = "AMQ_ACP_HEARTBEAT_INTERVAL"
)

const (
	defaultTurnTimeout       = 10 * time.Minute
	defaultIdleTimeout       = 15 * time.Minute
	defaultPollInterval      = 100 * time.Millisecond
	defaultHeartbeatInterval = 15 * time.Second
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
	Root              string
	Me                string
	To                string
	StateDir          string
	TurnTimeout       time.Duration
	IdleTimeout       time.Duration
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
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

	stateDir, err := resolveStateDir(root)
	if err != nil {
		return Config{}, err
	}
	turnTimeout, err := durationEnv(EnvTurnTimeout, defaultTurnTimeout)
	if err != nil {
		return Config{}, err
	}
	idleTimeout, err := durationEnv(EnvIdleTimeout, defaultIdleTimeout)
	if err != nil {
		return Config{}, err
	}
	pollInterval, err := durationEnv(EnvPollInterval, defaultPollInterval)
	if err != nil {
		return Config{}, err
	}
	heartbeatInterval, err := durationEnv(EnvHeartbeatInterval, defaultHeartbeatInterval)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Root:              root,
		Me:                me,
		To:                to,
		StateDir:          stateDir,
		TurnTimeout:       turnTimeout,
		IdleTimeout:       idleTimeout,
		PollInterval:      pollInterval,
		HeartbeatInterval: heartbeatInterval,
	}, nil
}

func resolveStateDir(root string) (string, error) {
	stateDir := strings.TrimSpace(os.Getenv(EnvStateDir))
	if stateDir == "" {
		return filepath.Join(root, "meta", "acp"), nil
	}
	if !filepath.IsAbs(stateDir) {
		return "", contextError("invalid %s=%q: state directory must be absolute", EnvStateDir, stateDir)
	}
	stateDir = filepath.Clean(stateDir)
	if !pathWithin(root, stateDir) {
		return "", contextError("invalid %s=%q: state directory must remain under %s", EnvStateDir, stateDir, EnvRoot)
	}
	return stateDir, nil
}

func pathWithin(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		if err == nil {
			err = fmt.Errorf("must be positive")
		}
		return 0, contextError("invalid %s=%q: %v", name, raw, err)
	}
	return duration, nil
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
