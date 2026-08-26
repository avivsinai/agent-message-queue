package adapter

import "regexp"

// lowercaseThreadUUIDRe matches an exact lowercase uuid (8-4-4-4-12 hex). It is
// anchored so no path-segment, query, or extra-component smuggling can reach a
// URL or argv built from the target. Shared by the Codex GUI deep-link seat
// (codex-app:thread:<uuid>) and the CLI queue seat (codex-queue:thread:<uuid>).
var lowercaseThreadUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
