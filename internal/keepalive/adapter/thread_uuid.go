package adapter

import (
	"fmt"
	"regexp"
	"strings"
)

// lowercaseThreadUUIDRe matches an exact lowercase uuid (8-4-4-4-12 hex). It is
// anchored so no path-segment, query, or extra-component smuggling can reach a
// URL or argv built from the target. Shared by the Codex GUI deep-link seat
// (codex-app:thread:<uuid>), the CLI queue seat (codex-queue:thread:<uuid>),
// and the Claude print seat (claude-print:session:<uuid>).
var lowercaseThreadUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// parseTargetUUID returns the uuid after prefix in a seat target. seat and
// noun name the target in errors ("Codex queue", "thread"); usage lists the
// accepted target forms.
func parseTargetUUID(target, prefix, seat, noun, usage string) (string, error) {
	id, ok := strings.CutPrefix(strings.TrimSpace(target), prefix)
	if !ok {
		return "", fmt.Errorf("unsupported %s target %q; use %s", seat, target, usage)
	}
	id = strings.TrimSpace(id)
	if !lowercaseThreadUUIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid %s %s uuid %q; want an exact lowercase 8-4-4-4-12 hex uuid", seat, noun, id)
	}
	return id, nil
}
