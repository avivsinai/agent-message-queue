package adapter

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// codexAppBundleID is the identity pin for the Codex app (rebranded ChatGPT.app)
// from docs/adr-wake-capability-vector.md. A Probe that cannot prove this
// bundle id owns the codex:// scheme fails closed.
const codexAppBundleID = "com.openai.codex"

// codexAppTargetNew and codexAppTargetThreadPrefix are the two stable target
// grammars. codex-app:new prefills a brand-new thread; codex-app:thread:<uuid>
// prefills the composer of an exact existing conversation identified by its
// rollout uuid (the trailing uuidv7 of a
// ~/.codex/sessions/YYYY/MM/DD/rollout-*-<uuid>.jsonl filename).
const (
	codexAppTargetNew          = "codex-app:new"
	codexAppTargetThreadPrefix = "codex-app:thread:"
)

// codexAppThreadUUIDRe matches an exact lowercase uuidv7 (8-4-4-4-12 hex). It
// is anchored so no path-segment, query, or extra-component smuggling can
// reach the URL built from the target.
var codexAppThreadUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CodexApp implements the honest deep-link seat for the Codex app
// (com.openai.codex): `codex://threads/new?prompt=<text>` prefills a NEW
// thread, and `codex://threads/<uuid>?prompt=<text>` opens the EXACT EXISTING
// conversation <uuid> and prefills its composer. Neither auto-submits; a human
// send is always required (verified live on app 26.818.61809). This adapter
// only launches the deep-link; it does not and cannot submit.
//
// Dispatch-vs-delivery caveat (live finding, issue #640): `open` exits 0 once
// the OS dispatches the URL to the registered handler — that proves DISPATCH
// only, not delivery. The app can refuse the deep-link with no adapter-visible
// signal: e.g. a thread with an ACTIVE WRITER shows an "Error creating chat —
// thread <uuid> already has an active writer" toast and leaves the composer
// empty, yet `open` still exits 0 and Inject reports success. Therefore the
// codex-app:thread:<uuid> target must name an IDLE conversation (no active
// writer), and callers must treat Inject success as "launched, not confirmed
// delivered". The adapter cannot observe the in-app refusal because the Codex
// app is AX-opaque (Chromium content is not exposed to System Events).
//
// The native `execute javascript` Apple Events path is DEAD and stays dead:
// live smoke on app 26.818.61809 returned `Access not allowed (-1723)`, and
// writing browser.allow_javascript_apple_events=true into the profile
// Preferences did not unlock it (the pref survives quit but the runtime check
// is compiled out). Do not re-try that path; the finding is recorded at
// https://github.com/avivsinai/agent-message-queue/issues/640#issuecomment-5406484999.
// The task-0 probe script in scripts/probe-codex-app-execute-javascript.sh
// remains as historical evidence and is intentionally not invoked here.
type CodexApp struct {
	Runner CommandRunner
}

func (CodexApp) Name() string {
	return "codex-app"
}

// CapabilityForTarget declares the per-target vector this deep-link seat can
// honestly claim. Both targets are launch + prefilled + requires_human; they
// differ only on session scope: codex-app:new is SessionNew, and
// codex-app:thread:<uuid> is SessionExistingExact (it opens a specific
// existing conversation). A caller requesting an existing-exact session is
// therefore refused the new-only target rather than silently downgraded.
func (CodexApp) CapabilityForTarget(target string) (Capability, error) {
	switch target {
	case codexAppTargetNew:
		return Capability{Activation: ActivationLaunch, Delivery: DeliveryPrefilled, Session: SessionNew, RequiresHuman: true}, nil
	default:
		if _, err := parseCodexAppThreadUUID(target); err != nil {
			return Capability{}, err
		}
		return Capability{Activation: ActivationLaunch, Delivery: DeliveryPrefilled, Session: SessionExistingExact, RequiresHuman: true}, nil
	}
}

func (CodexApp) NormalizeTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == codexAppTargetNew {
		return codexAppTargetNew, nil
	}
	if id, err := parseCodexAppThreadUUID(target); err == nil {
		return codexAppTargetThreadPrefix + id, nil
	} else {
		return "", err
	}
}

// Discover returns the new-thread seat (the only target that does not require
// a pre-existing conversation id). It is deliberately reachable so an explicit
// `attach --adapter codex-app --target codex-app:new` can name it; the
// human-handoff gate is enforced at registration, not here. Discovery does not
// auto-open the app.
func (c CodexApp) Discover(ctx context.Context) (string, error) {
	if err := requireDarwin(); err != nil {
		return "", err
	}
	if err := c.Probe(ctx, codexAppTargetNew); err != nil {
		return "", err
	}
	return codexAppTargetNew, nil
}

// Probe verifies the Codex app identity before blessing the target. It
// resolves the actual default handler of the `codex://` scheme and asserts it
// is the pinned bundle id com.openai.codex — the same app that
// `open codex://...` will dispatch to at write time. (The app also registers
// http/https; only the codex scheme is pinned.) A mismatch, a missing
// handler, a non-darwin platform, or any command failure fails closed.
func (c CodexApp) Probe(ctx context.Context, target string) error {
	if err := requireDarwin(); err != nil {
		return err
	}
	if _, err := c.NormalizeTarget(target); err != nil {
		return err
	}
	return probeSchemeOwner(ctx, c.runner(), "codex", codexAppBundleID)
}

// Inject launches the prefill-only deep-link. The payload becomes a properly
// query-escaped `prompt` parameter; the resulting URL is passed as a single
// argument to `open`. The prompt text never enters AppleScript/osascript or
// any script source — it exists only as the single query-escaped `open`
// argument. There is no System Events/keystroke/clipboard/activate, and
// crucially no submit — this seat never sends Enter.
//
// Identity is revalidated before the write: `open` is scheme-bound, not
// identity-bound (it dispatches to whatever app owns codex:// at call time),
// so Probe runs here, not only at Discover/registration. On any identity
// mismatch or command failure Inject fails closed without emitting the open.
func (c CodexApp) Inject(ctx context.Context, target string, payload string) error {
	if err := requireDarwin(); err != nil {
		return err
	}
	normalized, err := c.NormalizeTarget(target)
	if err != nil {
		return err
	}
	if err := c.Probe(ctx, normalized); err != nil {
		return fmt.Errorf("inject Codex app: %w", err)
	}
	u := url.URL{Scheme: "codex", Host: "threads"}
	switch normalized {
	case codexAppTargetNew:
		u.Path = "/new"
	default:
		u.Path = "/" + strings.TrimPrefix(normalized, codexAppTargetThreadPrefix)
	}
	q := url.Values{}
	q.Set("prompt", payload)
	u.RawQuery = q.Encode()
	out, err := c.runner().Run(ctx, "open", u.String())
	if err != nil {
		return fmt.Errorf("inject Codex app deep-link: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c CodexApp) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

// parseCodexAppThreadUUID extracts and validates the conversation uuid from a
// codex-app:thread:<uuid> target. It returns an error for anything that is not
// an exact lowercase 8-4-4-4-12 hex uuid, so no path-segment, query, or extra
// component can be smuggled into the URL built from the target.
func parseCodexAppThreadUUID(target string) (string, error) {
	id, ok := strings.CutPrefix(strings.TrimSpace(target), codexAppTargetThreadPrefix)
	if !ok {
		return "", fmt.Errorf("unsupported Codex app target %q; use %q or %s<uuid>", target, codexAppTargetNew, codexAppTargetThreadPrefix)
	}
	id = strings.TrimSpace(id)
	if !codexAppThreadUUIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid Codex app thread uuid %q; want an exact lowercase 8-4-4-4-12 hex uuid", id)
	}
	return id, nil
}
