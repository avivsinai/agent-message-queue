package claude

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/claudepeer"
)

// Pure peer framing lives in internal/claudepeer; these aliases preserve
// the Remote-facing symbols. Key-file discovery below stays Remote-owned.

// Frame is one peer message frame (capture §3).
type Frame = claudepeer.Frame

// BuildCrossSessionEnvelope mirrors the capture's pQe builder (§3.1).
func BuildCrossSessionEnvelope(from, fromSession, body, hopChain, fromName, fromMode string) (string, error) {
	return claudepeer.BuildCrossSessionEnvelope(from, fromSession, body, hopChain, fromName, fromMode)
}

// crossSessionBodies is the inverse of BuildCrossSessionEnvelope for the
// transcript.
func crossSessionBodies(text string) (string, bool) {
	return claudepeer.CrossSessionBodies(text)
}

// NewMsgID returns a fresh random v4-formatted UUID for msg_id.
func NewMsgID() (string, error) {
	return claudepeer.NewMsgID()
}

// BuildFrame assembles the deliverable frame (capture §3).
func BuildFrame(from, body string) (*Frame, error) {
	return claudepeer.BuildFrame(from, body)
}

// EncodeFrames renders the mandatory auth line and the frame line with
// compact separators, each \n-terminated — the pinned line-delimited JSON.
func EncodeFrames(token string, f *Frame) ([]byte, error) {
	return claudepeer.EncodeFrames(token, f)
}

// peerKeyFile resolves the target's key-file name (capture §2):
// <pid>.<sha256(path.resolve(sockPath))>.key under ~/.claude/sessions.
//
// Node's path.resolve is LEXICAL: absolute + normalized, with no symlink
// resolution (docs/research/r0-03-cc-socket-wire-capture.md:41, live
// verified). filepath.Abs is the same operation. EvalSymlinks must never
// be applied here: on macOS /tmp is a symlink to /private/tmp, so a
// resolved hash names a key file the target never minted and every real
// target reads as "no inbound" (reproduced live).
func peerKeyFile(home string, pid int, sockPath string) (string, error) {
	abs, err := filepath.Abs(sockPath)
	if err != nil {
		return "", err
	}
	h := sha256Hex([]byte(abs))
	return filepath.Join(claudeSessionsDir(home), fmt.Sprintf("%d.%s.key", pid, h)), nil
}

// maxPeerKeyBytes bounds the key-file read: the real file is a one-line
// JSON object well under 1 KiB.
const maxPeerKeyBytes = 8 << 10

func readPeerToken(home string, pid int, sockPath string) (string, error) {
	p, err := peerKeyFile(home, pid, sockPath)
	if err != nil {
		return "", err
	}
	// Bounded, no-follow, non-blocking read: the key file lives in a
	// directory the target harness owns; a FIFO or a huge
	// file there must not stall or bloat Submit.
	raw, err := readRegularBounded(p, maxPeerKeyBytes)
	if err != nil {
		return "", fmt.Errorf("peer key %s: %w", p, err)
	}
	var k struct {
		PeerToken string `json:"peerToken"`
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return "", fmt.Errorf("peer key %s: %w", p, err)
	}
	if !claudepeer.PeerTokenValid(k.PeerToken) {
		return "", fmt.Errorf("peer key %s: peerToken does not match the pinned shape", p)
	}
	return k.PeerToken, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}
