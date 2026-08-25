package adapter

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// validSchemeRe pins the shape of a URL scheme accepted by
// schemeOwnerProbeScript. Both call sites pass fixed literals ("claude",
// "codex"), but the scheme is concatenated into Swift source, so an unanchored
// or caller-supplied value could inject. This makes the invalid state
// unrepresentable: a scheme that is not ^[a-z][a-z0-9+.-]*$ panics at the call
// site rather than reaching the script builder.
var validSchemeRe = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// schemeOwnerProbeScript returns a fixed Swift one-liner (no payload) that
// resolves the actual default handler of the given URL scheme and prints its
// bundle id, or an empty string if no handler is registered. It pins the
// scheme owner — the app that `open <scheme>://...` dispatches to — not an
// app looked up by name, so a stale or conflicting scheme registration fails
// closed instead of blessing the wrong app.
//
// Swift (not JXA) is used because LSCopyDefaultApplicationURLForURL returns
// NULL for an unregistered scheme and Swift's Optional handling is
// crash-proof there, where JXA crashes (a native memory fault that JXA
// try/catch cannot intercept). The scheme is a fixed literal per adapter, never
// caller-supplied, so it cannot inject into the script source.
func schemeOwnerProbeScript(scheme string) string {
	if !validSchemeRe.MatchString(scheme) {
		panic(fmt.Sprintf("probeSchemeOwner: invalid scheme %q (must match ^[a-z][a-z0-9+.-]*$)", scheme))
	}
	return `import CoreServices; import Foundation; let u=URL(string:"` + scheme + `://x") as CFURL?; if let r=LSCopyDefaultApplicationURLForURL(u!,.all,nil){print(Bundle(url:r.takeRetainedValue() as URL)?.bundleIdentifier ?? "")}else{print("")}`
}

// probeSchemeOwner resolves the default handler bundle id for scheme through
// runner and asserts it equals wantBundleID. Used by deep-link adapters
// (claude-desktop, codex-app) to pin the scheme owner before the `open` write.
// A mismatch, a missing handler, a non-darwin platform, or any command failure
// fails closed.
func probeSchemeOwner(ctx context.Context, runner CommandRunner, scheme, wantBundleID string) error {
	if err := requireDarwin(); err != nil {
		return err
	}
	out, err := runner.Run(ctx, "swift", "-e", schemeOwnerProbeScript(scheme))
	if err != nil {
		return fmt.Errorf("probe %s:// scheme handler: %w: %s", scheme, err, strings.TrimSpace(string(out)))
	}
	got := strings.TrimSpace(string(out))
	if got != wantBundleID {
		return fmt.Errorf("%s:// scheme handler is %q, want %q", scheme, got, wantBundleID)
	}
	return nil
}
