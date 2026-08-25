package adapter

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

const claudeDesktopTarget = "claude-desktop:new"

// claudeDesktopBundleID is the identity pin for Claude Desktop from
// docs/adr-wake-capability-vector.md. A Probe that cannot prove this bundle id
// owns the claude:// scheme fails closed.
const claudeDesktopBundleID = "com.anthropic.claudefordesktop"

// ClaudeDesktop implements the honest deep-link seat from the wake capability
// ADR: prefilled + new + requires_human. `claude://code/new?q=<prompt>`
// PREFILLS the composer on the Code surface and never auto-submits; a human
// send is always required (verified live on Claude Desktop 1.34493.1). This
// adapter only launches the deep-link; it does not and cannot submit.
type ClaudeDesktop struct {
	Runner CommandRunner
}

func (ClaudeDesktop) Name() string {
	return "claude-desktop"
}

// Capability declares the proven seat vector: the deep-link launches the app
// (ActivationLaunch), prefills the composer without submitting
// (DeliveryPrefilled), always targets a new session (SessionNew), and a human
// must press send (RequiresHuman).
func (ClaudeDesktop) Capability() Capability {
	return Capability{
		Activation:    ActivationLaunch,
		Delivery:      DeliveryPrefilled,
		Session:       SessionNew,
		RequiresHuman: true,
	}
}

func (ClaudeDesktop) NormalizeTarget(target string) (string, error) {
	if strings.TrimSpace(target) != claudeDesktopTarget {
		return "", fmt.Errorf("unsupported Claude Desktop target %q; only the new-session seat is stable", target)
	}
	return claudeDesktopTarget, nil
}

// Discover returns the single new-session seat. It is deliberately reachable
// so an explicit `attach --adapter claude-desktop --target claude-desktop:new`
// can name it; the human-handoff gate is enforced at registration
// (CapabilityDeclarer), not here. Discovery does not auto-open the app.
func (c ClaudeDesktop) Discover(ctx context.Context) (string, error) {
	if err := requireDarwin(); err != nil {
		return "", err
	}
	if err := c.Probe(ctx, claudeDesktopTarget); err != nil {
		return "", err
	}
	return claudeDesktopTarget, nil
}

// Probe verifies the Claude Desktop app identity before blessing the target.
// It resolves the actual default handler of the `claude://` scheme and asserts
// it is the pinned bundle id com.anthropic.claudefordesktop — the same app
// that `open claude://...` will dispatch to at write time. A mismatch, a
// missing handler, a non-darwin platform, or any command failure fails closed.
func (c ClaudeDesktop) Probe(ctx context.Context, target string) error {
	if err := requireDarwin(); err != nil {
		return err
	}
	if _, err := c.NormalizeTarget(target); err != nil {
		return err
	}
	return probeSchemeOwner(ctx, c.runner(), "claude", claudeDesktopBundleID)
}

// Inject launches the prefill-only deep-link. The payload becomes a properly
// query-escaped `q` parameter; the resulting URL is passed as a single
// argument to `open`. The prompt text never enters AppleScript/osascript or
// any script source — it exists only as the single query-escaped `open`
// argument. There is no System Events/keystroke/clipboard/activate, and
// crucially no submit — this seat never sends Enter.
//
// Identity is revalidated before the write: `open` is scheme-bound, not
// identity-bound (it dispatches to whatever app owns claude:// at call time),
// so Probe runs here, not only at Discover/registration. On any identity
// mismatch or command failure Inject fails closed without emitting the open.
func (c ClaudeDesktop) Inject(ctx context.Context, target string, payload string) error {
	if err := requireDarwin(); err != nil {
		return err
	}
	if _, err := c.NormalizeTarget(target); err != nil {
		return err
	}
	if err := c.Probe(ctx, target); err != nil {
		return fmt.Errorf("inject Claude Desktop: %w", err)
	}
	u := url.URL{Scheme: "claude", Host: "code", Path: "/new"}
	q := url.Values{}
	q.Set("q", payload)
	u.RawQuery = q.Encode()
	out, err := c.runner().Run(ctx, "open", u.String())
	if err != nil {
		return fmt.Errorf("inject Claude Desktop deep-link: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c ClaudeDesktop) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}
