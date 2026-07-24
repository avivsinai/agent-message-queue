package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type sessionPin struct {
	Present      bool
	Session      string
	BaseRoot     string
	ExpectedRoot string
	BaseRootID   string
	RootID       string
	IdentityPin  bool
}

// loadSessionPin distinguishes an absent legacy pin from an explicitly empty
// base-root pin. New shell writers always replace the complete context set.
func loadSessionPin() (sessionPin, error) {
	rawSession, present := os.LookupEnv(envSession)
	_, rootIDPresent := os.LookupEnv(envRootID)
	_, baseRootIDPresent := os.LookupEnv(envBaseRootID)
	if !present && !rootIDPresent && !baseRootIDPresent {
		return sessionPin{}, nil
	}

	pin := sessionPin{Present: true, Session: strings.TrimSpace(rawSession)}
	if pin.Session != "" {
		if err := validateSessionName(pin.Session); err != nil {
			return sessionPin{}, ContextMismatchError("invalid %s=%q: %v", envSession, rawSession, err)
		}
	}

	base := strings.TrimSpace(os.Getenv(envBaseRoot))
	if base != "" {
		if !filepath.IsAbs(base) {
			return sessionPin{}, ContextMismatchError("invalid %s=%q: pinned base root must be absolute", envBaseRoot, base)
		}
		pin.BaseRoot = absPath(filepath.Clean(base))
	}

	if pin.BaseRoot == "" {
		return sessionPin{}, ContextMismatchError("incomplete AMQ session pin: %s=%q requires an exact %s", envSession, pin.Session, envBaseRoot)
	}
	if pin.Session != "" {
		pin.ExpectedRoot = filepath.Join(pin.BaseRoot, pin.Session)
	} else {
		pin.ExpectedRoot = pin.BaseRoot
	}

	rootID, rootIDPresent := os.LookupEnv(envRootID)
	baseRootID, baseRootIDPresent := os.LookupEnv(envBaseRootID)
	if !rootIDPresent && !baseRootIDPresent {
		return pin, nil
	}
	rootID = strings.TrimSpace(rootID)
	baseRootID = strings.TrimSpace(baseRootID)
	if !rootIDPresent || !baseRootIDPresent || rootID == "" || baseRootID == "" {
		return sessionPin{}, ContextMismatchError("incomplete AMQ identity pin: %s and %s must both be present and non-empty", envRootID, envBaseRootID)
	}
	if !validTreeIdentityToken(rootID) || !validTreeIdentityToken(baseRootID) {
		return sessionPin{}, ContextMismatchError("unverifiable AMQ identity pin: unsupported or malformed %s/%s", envRootID, envBaseRootID)
	}
	pin.RootID = rootID
	pin.BaseRootID = baseRootID
	pin.IdentityPin = true
	return pin, nil
}

// verifyRootUnderBase authenticates the base and proves that session is a
// direct, non-symlink child of it before authenticating the resulting root.
func verifyRootUnderBase(base, baseID, session, root, rootID string) error {
	if verifyTreeIdentityToken(base, baseID) != TreeRelationSame {
		return ContextMismatchError("pinned base root identity is not current: %s", base)
	}
	base = absPath(resolveRoot(base))
	root = absPath(resolveRoot(root))
	if session == "" {
		if !sameTreeIdentity(base, root) {
			return ContextMismatchError("root is not the pinned base root")
		}
	} else {
		entry := filepath.Join(base, session)
		info, err := os.Lstat(entry)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ContextMismatchError("pinned session %q is not a direct directory under base", session)
		}
		if !sameTreeIdentity(entry, root) {
			return ContextMismatchError("root is not the pinned session directory")
		}
	}
	if verifyTreeIdentityToken(root, rootID) != TreeRelationSame {
		return ContextMismatchError("target root identity is not current: %s", root)
	}
	return nil
}

func sessionPinMismatch(target string) (*SessionContextError, error) {
	target = absPath(resolveRoot(target))
	pin, err := loadSessionPin()
	if err != nil {
		return nil, err
	}
	if pin.Present {
		if pin.IdentityPin {
			if err := verifyRootUnderBase(pin.BaseRoot, pin.BaseRootID, pin.Session, target, pin.RootID); err != nil {
				return &SessionContextError{Message: err.Error()}, nil
			}
			return nil, nil
		}
		expected := pin.ExpectedRoot
		if target == expected {
			return nil, nil
		}
		return &SessionContextError{Message: fmt.Sprintf(
			"session context mismatch: target root %s differs from pinned root %s (AM_SESSION=%q)",
			target, expected, pin.Session,
		)}, nil
	}

	if source, ok := conflictingSourceRoot(target); ok {
		return &SessionContextError{Message: fmt.Sprintf(
			"session context mismatch: target root %s belongs to a different AMQ tree than established root %s",
			target, source,
		)}, nil
	}
	return nil, nil
}

// resolveMailboxRoot makes --session a routing operation. With a pin, the
// target is always constructed from the authorized base rather than ambient
// AM_ROOT. Without a pin, an explicit session remains deliberate legacy input.
func resolveMailboxRoot(common *commonFlags, rawSession string) (root string, routed bool, err error) {
	root = absPath(resolveRoot(common.Root))
	session := strings.TrimSpace(rawSession)
	if session == "" {
		return root, false, nil
	}
	if common.rootExplicit() {
		return "", false, UsageError("--session and --root are mutually exclusive")
	}
	if err := validateSessionName(session); err != nil {
		return "", false, UsageError("--session: %v", err)
	}

	pin, err := loadSessionPin()
	if err != nil {
		return "", false, err
	}
	base := ""
	switch {
	case pin.Present:
		if pin.IdentityPin {
			if err := verifyRootUnderBase(pin.BaseRoot, pin.BaseRootID, pin.Session, root, pin.RootID); err != nil {
				return "", false, err
			}
			// The requested session must be a direct, non-symlink child of the
			// authenticated base; do not route through an attacker-controlled alias.
			if verifyTreeIdentityToken(pin.BaseRoot, pin.BaseRootID) != TreeRelationSame {
				return "", false, ContextMismatchError("refusing routed session: pinned base root identity is not current")
			}
			entry := filepath.Join(pin.BaseRoot, session)
			info, e := os.Lstat(entry)
			if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", false, ContextMismatchError("refusing routed session: %q is not a direct directory under pinned base", session)
			}
		}
		base = pin.BaseRoot
	default:
		base = baseRootOf(root)
	}

	target, err := resolveSessionRoot(base, session)
	if err != nil {
		return "", false, err
	}
	return target, true, nil
}

// resolveSessionRoot resolves a session beneath base using canonical paths. This
// prevents a symlinked session (or base) from bypassing tree/session identity
// checks performed by callers.
func resolveSessionRoot(base, session string) (string, error) {
	base = absPath(resolveRoot(base))
	if !dirExists(base) {
		return "", NotFoundError("base root not found at %s", base)
	}
	target := absPath(filepath.Join(base, session))
	entry, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", NotFoundError("session %q not found at %s", session, target)
		}
		return "", ContextMismatchError("cannot inspect session %q: %v", session, err)
	}
	if !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
		return "", ContextMismatchError("session %q is not a direct directory under base", session)
	}
	canonBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	canonTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(canonBase, canonTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ContextMismatchError("session %q resolves outside base root", session)
	}
	// Preserve the caller's lexical root for mailbox layout and diagnostics;
	// canonical paths above are used only for containment validation.
	return target, nil
}

func guardMailboxContext(command, target string, routed, ignorePin bool) error {
	if routed || ignorePin {
		return nil
	}
	mismatch, err := sessionPinMismatch(target)
	if err != nil {
		return err
	}
	if mismatch == nil {
		return nil
	}
	return ContextMismatchError("refusing %s: %s. Use --session <name> to route deliberately, or explicit --root with --ignore-session-pin", command, mismatch.Error())
}

func guardPinnedSourceContext(command, target string, ignorePin bool) error {
	if ignorePin {
		return nil
	}
	mismatch, err := sessionPinMismatch(target)
	if err != nil {
		return err
	}
	if mismatch == nil {
		return nil
	}
	return ContextMismatchError("refusing %s: %s. Target routing does not authorize a mismatched source; use an explicit source route, or explicit --root with --ignore-session-pin", command, mismatch.Error())
}

func validatePinOverride(common *commonFlags, ignorePin bool, routed bool) error {
	if !ignorePin {
		return nil
	}
	if routed {
		return UsageError("--ignore-session-pin cannot be used with --session")
	}
	if !common.rootExplicit() {
		return UsageError("--ignore-session-pin requires an explicit --root")
	}
	return nil
}

func requireMailbox(root, me string) error {
	for _, dir := range []string{
		fsq.AgentInboxNew(root, me),
		fsq.AgentInboxCur(root, me),
		fsq.AgentDLQNew(root, me),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return NotFoundError("mailbox for %q is missing at root %s (missing %s); check AM_ROOT or use --session <name>", me, root, dir)
			}
			return err
		}
		if !info.IsDir() {
			return NotFoundError("mailbox path for %q is not a directory: %s", me, dir)
		}
	}
	return nil
}
