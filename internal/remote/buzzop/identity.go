// Package buzzop holds the values a Buzz frontend and the machine agent's
// engine share. It does no I/O.
package buzzop

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// Identity is the managed agent's public Buzz identity, fixed when the
// worker starts. It holds no secret, so it is safe to log.
//
// RelayURL is the normalized relay URL (NormalizeRelay) and RelayKey its
// state directory key (RelayKey); both are "" without a usable URL.
// RelaySelf is the relay's own public key; "" means facts the relay signs
// are unknown. Agent is the public key of the credential the transport
// holds (64 lowercase hex), or "" when that secret is absent or unreadable.
// Owner is the agent's owner public key, or "" when unknown. DisplayName is
// the agent's name as Desktop passed it.
type Identity struct {
	RelayURL    string
	RelayKey    string
	RelaySelf   string
	Owner       string
	Agent       string
	DisplayName string
}

// NormalizeRelay is a relay URL with its scheme and host lowercased and no
// trailing slash, query or fragment; "" when it is not an absolute URL.
func NormalizeRelay(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
}

// RelayKey is the directory key of a relay URL.
func RelayKey(raw string) string {
	norm := NormalizeRelay(raw)
	if norm == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}
