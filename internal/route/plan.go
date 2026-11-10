package route

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/exitcode"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// ErrPeerRootUnreachable is the sentinel that marks a routing failure as
// TRANSIENT: the peer root does not exist YET, or is not accessible YET.
// ResolveReplyRoute wraps these errors in amqio.TransientRouteError so the
// carrier leaves the message in new for the next tick instead of DLQ'ing it.
// Unknown-project and malformed-handle errors are NOT wrapped — they are
// poison.
var ErrPeerRootUnreachable = errors.New("peer delivery root is not reachable")

// ResolveSessionRoot resolves a session beneath base using canonical paths. This
// prevents a symlinked session (or base) from bypassing tree/session identity
// checks performed by callers.
func (w Walker) ResolveSessionRoot(base, session string) (string, error) {
	base = absPath(w.ResolveRoot(base))
	if !dirExists(base) {
		return "", exitcode.NotFoundError("base root not found at %s", base)
	}
	target := absPath(filepath.Join(base, session))
	entry, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", exitcode.NotFoundError("session %q not found at %s", session, target)
		}
		// A non-ENOENT Lstat on the session directory (EACCES/EIO/ESTALE)
		// is transient, not a permanent context mismatch. Classify it as
		// ErrPeerRootUnreachable so retryableRouteError rescues it and the carrier
		// retries instead of DLQing the command. Actual symlink, non-directory, and
		// containment violations (below) remain ContextMismatchError.
		return "", fmt.Errorf("%w: cannot inspect session %q: %v", ErrPeerRootUnreachable, session, err)
	}
	if !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
		return "", exitcode.ContextMismatchError("session %q is not a direct directory under base", session)
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
		return "", exitcode.ContextMismatchError("session %q resolves outside base root", session)
	}
	// Preserve the caller's lexical root for mailbox layout and diagnostics;
	// canonical paths above are used only for containment validation.
	return target, nil
}

// NormalizeHandle trims raw and validates it as a handle.
func NormalizeHandle(raw string) (string, error) {
	handle := strings.TrimSpace(raw)
	if strings.ContainsAny(handle, "/\\") {
		return "", fmt.Errorf("invalid handle (slashes not allowed): %s", handle)
	}
	if err := fsq.ValidateHandle(handle); err != nil {
		return "", err
	}
	return handle, nil
}

// ParseReplyTo reads a reply_to value: handle@session, or a bare handle when
// allowBase is set.
func ParseReplyTo(raw string, allowBase bool) (handle, session string, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", fmt.Errorf("malformed reply_to %q: value is empty", raw)
	}
	if value != raw || strings.Count(value, "@") > 1 {
		return "", "", fmt.Errorf("malformed reply_to %q: expected handle or handle@session", raw)
	}

	parts := strings.Split(value, "@")
	if len(parts) == 1 {
		if !allowBase {
			return "", "", fmt.Errorf("malformed reply_to %q: expected handle@session", raw)
		}
	} else if parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed reply_to %q: expected non-empty handle@session", raw)
	}

	handle, err = NormalizeHandle(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("invalid handle in reply_to %q: %w", raw, err)
	}
	if len(parts) == 1 {
		return handle, "", nil
	}
	session, err = NormalizeHandle(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("invalid session in reply_to %q: %w", raw, err)
	}
	return handle, session, nil
}

// DeliveryPlan is where a send to a project and session delivers.
type DeliveryPlan struct {
	DeliveryRoot     string
	PeerBaseRoot     string
	SourceConfigPath string
	SourceProject    string
	TargetProject    string
	TargetSession    string
}

// DeliveryOptions tunes PlanDelivery.
type DeliveryOptions struct {
	// MirrorPeerSession delivers a cross-project send from a session root to
	// the peer's session of the same name.
	MirrorPeerSession bool
}

// PlanDelivery plans where a send from sourceRoot to targetProject and
// targetSession delivers.
func (w Walker) PlanDelivery(sourceRoot, targetProject, targetSession string, opts DeliveryOptions) (DeliveryPlan, error) {
	plan := DeliveryPlan{
		DeliveryRoot:  sourceRoot,
		TargetProject: strings.TrimSpace(targetProject),
		TargetSession: strings.TrimSpace(targetSession),
	}

	if plan.TargetProject != "" {
		routeConfig, err := w.FindAmqrcForRoot(sourceRoot)
		if err != nil {
			return plan, federationSourceProjectError(sourceRoot)
		}
		plan.SourceConfigPath = routeConfig.Path
		sourceProject, err := w.requireFederationSourceProject(routeConfig, sourceRoot)
		if err != nil {
			return plan, err
		}
		plan.SourceProject = sourceProject

		peerBaseRoot, err := routeConfig.Peer(targetProject)
		if err != nil {
			return plan, err
		}
		plan.PeerBaseRoot = peerBaseRoot

		if plan.TargetSession != "" {
			normalized, err := NormalizeHandle(plan.TargetSession)
			if err != nil {
				return plan, exitcode.UsageError("--session: %v", err)
			}
			plan.TargetSession = normalized
			plan.DeliveryRoot, err = w.ResolveSessionRoot(peerBaseRoot, plan.TargetSession)
			if err != nil {
				if !dirExists(peerBaseRoot) {
					return plan, PeerDeliveryRootError(plan, peerBaseRoot, err)
				}
				return plan, err
			}
		} else if opts.MirrorPeerSession && w.ClassifyRoot(sourceRoot) != "" {
			plan.TargetSession = filepath.Base(sourceRoot)
			plan.DeliveryRoot, err = w.ResolveSessionRoot(peerBaseRoot, plan.TargetSession)
			if err != nil {
				if !dirExists(peerBaseRoot) {
					return plan, PeerDeliveryRootError(plan, peerBaseRoot, err)
				}
				return plan, err
			}
		} else {
			plan.DeliveryRoot = peerBaseRoot
		}
		return plan, nil
	}

	if plan.TargetSession != "" {
		normalized, err := NormalizeHandle(plan.TargetSession)
		if err != nil {
			return plan, exitcode.UsageError("--session: %v", err)
		}
		plan.TargetSession = normalized
		baseRoot := w.ClassifyRoot(sourceRoot)
		if baseRoot == "" {
			return plan, exitcode.UsageError("--session requires a session context: run from inside 'amq coop exec --session <name>'")
		}
		plan.DeliveryRoot, err = w.ResolveSessionRoot(baseRoot, plan.TargetSession)
		if err != nil {
			return plan, err
		}
	}

	return plan, nil
}

// PeerDeliveryRootError reports that the peer delivery root of plan cannot
// be accessed, as ErrPeerRootUnreachable.
func PeerDeliveryRootError(plan DeliveryPlan, deliveryRoot string, cause error) error {
	configPath := plan.SourceConfigPath
	if configPath == "" {
		configPath = "the selected source .amqrc"
	}
	return fmt.Errorf(
		"%w: cannot access selected peer %q delivery root %s: %v; check that the peer root exists and is accessible; if the peer moved, fix the %q path in the peers map of .amqrc at %s, then retry the command",
		ErrPeerRootUnreachable,
		plan.TargetProject,
		deliveryRoot,
		cause,
		plan.TargetProject,
		configPath,
	)
}

func (w Walker) requireFederationSourceProject(result AmqrcFile, sourceRoot string) (string, error) {
	project := strings.TrimSpace(result.Project())
	if project == "" {
		return "", federationSourceProjectError(sourceRoot)
	}
	configuredRoot := strings.TrimSpace(result.Config.Root)
	if configuredRoot == "" {
		return "", federationSourceOwnershipError(result, sourceRoot, "")
	}
	configuredBase, err := result.PeerPath(configuredRoot)
	if err != nil {
		return "", fmt.Errorf("resolve configured source root: %w", err)
	}
	sourceRoot = absPath(w.ResolveRoot(sourceRoot))
	configuredBase = absPath(w.ResolveRoot(configuredBase))
	if !w.sameTreeOrPath(sourceRoot, configuredBase) {
		if !w.sameTreeOrPath(filepath.Dir(sourceRoot), configuredBase) {
			return "", federationSourceOwnershipError(result, sourceRoot, configuredBase)
		}
		ownedSession, err := w.ResolveSessionRoot(configuredBase, filepath.Base(sourceRoot))
		if err != nil || !w.sameTreeOrPath(ownedSession, sourceRoot) {
			return "", federationSourceOwnershipError(result, sourceRoot, configuredBase)
		}
	}
	return project, nil
}

// sameTreeOrPath reports whether a and b are the same cleaned path or name the
// same directory tree. An unresolvable path is never accepted on identity
// alone; only the exact string match holds for a directory that does not
// exist yet.
func (w Walker) sameTreeOrPath(a, b string) bool {
	return a == b || w.RelateTrees(a, b) == TreeRelationSame
}

func federationSourceProjectError(sourceRoot string) error {
	return fmt.Errorf(
		"cross-project routing requires a non-empty source project identity; set \"project\" in the .amqrc that owns source root %s",
		sourceRoot,
	)
}

func federationSourceOwnershipError(result AmqrcFile, sourceRoot, configuredRoot string) error {
	if configuredRoot == "" {
		configuredRoot = "<empty>"
	}
	return fmt.Errorf(
		"configured .amqrc at %s does not own source root %s (configured root: %s)",
		result.Path,
		sourceRoot,
		configuredRoot,
	)
}

// retryableRouteError marks a destination that does not exist YET as
// ErrPeerRootUnreachable, so the carrier retries instead of dead-lettering.
// ResolveSessionRoot and the peer planner report absence as ExitNotFound;
// malformed names and unknown projects are other error kinds and stay
// permanent.
func retryableRouteError(err error) error {
	if errors.Is(err, ErrPeerRootUnreachable) {
		return err
	}
	var ecerr *exitcode.ExitCodeError
	if errors.As(err, &ecerr) && ecerr.Code == exitcode.ExitNotFound {
		return fmt.Errorf("%w: %v", ErrPeerRootUnreachable, err)
	}
	return err
}

// ResolveReplyRoute resolves where a cross-project reply must be delivered.
// The remote endpoint's AMQ carrier must answer a caller in the CALLER's
// root, but it has no business knowing about .amqrc discovery, peer maps or
// session layout. cmd/amq-remote injects this function into the carrier, so
// the carrier depends on a resolution contract rather than on configuration.
//
// sourceRoot is the endpoint's own root (where the command arrived).
// replyProject and replyTo come from the command message's reply_project and
// reply_to headers, which amq send stamps on every cross-project send.
// The returned root is where the reply must be written and handle is the
// mailbox inside it.
func (w Walker) ResolveReplyRoute(sourceRoot, replyProject, replyTo string) (root, handle string, err error) {
	project := strings.TrimSpace(replyProject)
	recipient, session, err := ParseReplyTo(replyTo, true)
	if err != nil {
		return "", "", fmt.Errorf("malformed reply metadata: %w", err)
	}
	if project == "" {
		// Empty reply_project does NOT mean the caller is in our own root.
		// The caller may be in a DIFFERENT SESSION of the same project. If
		// reply_to carries a session component (handle@session), route to
		// that session's root under our base root.
		if session == "" {
			return "", "", fmt.Errorf("reply_project is empty and reply_to has no session; not a cross-session reply")
		}
		// ClassifyRoot returns "" for a BASE root. A non-session source IS
		// the base. Use it directly so an endpoint at the base root can
		// route to a session root.
		base := w.ClassifyRoot(sourceRoot)
		if base == "" {
			base = sourceRoot
		}
		sessionRoot, err := w.ResolveSessionRoot(base, session)
		if err != nil {
			return "", "", retryableRouteError(err)
		}
		return sessionRoot, recipient, nil
	}
	plan, err := w.PlanDelivery(sourceRoot, project, session, DeliveryOptions{})
	if err != nil {
		// The router declares whether a failure is transient; the adapter in
		// cmd/amq-remote/serve.go turns ErrPeerRootUnreachable into
		// amqio.TransientRouteError. A peer session that does not exist YET is
		// as retryable as a peer base root that does not exist yet; the planner
		// marks only the latter.
		return "", "", retryableRouteError(err)
	}
	return plan.DeliveryRoot, recipient, nil
}
