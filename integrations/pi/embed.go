// Package pibridge carries the pi extension source, so an installed
// amq-remote hands out the bridge that its own protocol code expects.
package pibridge

import _ "embed"

// Source is integrations/pi/amq-bridge.ts as built into this binary.
//
//go:embed amq-bridge.ts
var Source []byte
