package claude

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Wire constants pinned by docs/research/r0-03-cc-socket-wire-capture.md
// (merged 17bff18). PR2 implements exactly this wire; no fresh probe.

// Frame is one peer message frame (capture §3).
type Frame struct {
	MsgV            int       `json:"msgV"`
	MsgID           string    `json:"msg_id"`
	Type            string    `json:"type"`
	Message         frameBody `json:"message"`
	Priority        string    `json:"priority"`
	From            string    `json:"from,omitempty"`
	FileAttachments []any     `json:"file_attachments,omitempty"`
}

type frameBody struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// authLine is the optional first line (capture §2).
type authLine struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// peerTokenRe is the pinned token shape: exactly 32 lowercase hex.
var peerTokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// BuildCrossSessionEnvelope mirrors the capture's pQe builder (§3.1):
// `^<cross-session-message[^>]*>\n<body>\n</cross-session-message>$` —
// exactly one \n after the open tag and before the close tag; attribute
// order from, from-session, hop-chain, from-name, from-mode; from/
// from-name restricted to the safe class [a-z0-9-]. The receiver re-
// serializes and compares byte-for-byte, so any drift silently drops the
// frame — the builder is total over its inputs and refuses unsafe ones
// rather than emitting a frame the receiver would discard.
func BuildCrossSessionEnvelope(from, fromSession, body, hopChain, fromName, fromMode string) (string, error) {
	for name, v := range map[string]string{"from": from, "from-name": fromName} {
		if !safeAttr(v) {
			return "", fmt.Errorf("envelope attr %s %q contains characters outside [a-z0-9-]", name, v)
		}
	}
	if strings.ContainsAny(body, "\x00") || strings.Contains(body, "</cross-session-message>") {
		return "", fmt.Errorf("envelope body contains a terminator sequence")
	}
	var b strings.Builder
	b.WriteString("<cross-session-message")
	if from != "" {
		b.WriteString(` from="` + from + `"`)
	}
	if fromSession != "" {
		b.WriteString(` from-session="` + fromSession + `"`)
	}
	if hopChain != "" {
		b.WriteString(` hop-chain="` + hopChain + `"`)
	}
	if fromName != "" {
		b.WriteString(` from-name="` + fromName + `"`)
	}
	if fromMode != "" {
		b.WriteString(` from-mode="` + fromMode + `"`)
	}
	b.WriteString(">\n")
	b.WriteString(body)
	b.WriteString("\n</cross-session-message>")
	return b.String(), nil
}

func safeAttr(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// NewMsgID returns a fresh random v4-formatted UUID for msg_id.
func NewMsgID() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}

// BuildFrame assembles the deliverable frame (capture §3).
func BuildFrame(from, body string) (*Frame, error) {
	env, err := BuildCrossSessionEnvelope(from, "amq", body, "", "amq-remote", "code")
	if err != nil {
		return nil, err
	}
	id, err := NewMsgID()
	if err != nil {
		return nil, err
	}
	return &Frame{
		MsgV:     1,
		MsgID:    id,
		Type:     "user",
		Message:  frameBody{Role: "user", Content: env},
		Priority: "next",
		From:     from,
	}, nil
}

// EncodeFrames renders the mandatory auth line and the frame line with
// compact separators, each \n-terminated — the pinned line-delimited JSON.
func EncodeFrames(token string, f *Frame) ([]byte, error) {
	var out []byte
	// The auth line is mandatory: an empty token is a fail-open shape
	// (architect PR2 note) — frames with no auth line must never be
	// emitted, so an empty token is refused, not skipped.
	if !peerTokenRe.MatchString(token) {
		return nil, fmt.Errorf("peer token does not match the pinned [0-9a-f]{32} shape")
	}
	a, err := json.Marshal(authLine{Type: "auth", Token: token})
	if err != nil {
		return nil, err
	}
	out = append(out, a...)
	out = append(out, '\n')
	line, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	out = append(out, line...)
	out = append(out, '\n')
	return out, nil
}

// peerKeyFile resolves the target's key-file name (capture §2):
// <pid>.<sha256(resolve(sockPath))>.key under ~/.claude/sessions.
func peerKeyFile(home string, pid int, sockPath string) (string, error) {
	// path.resolve = absolute + symlink-cleaned (EvalSymlinks after Abs).
	abs, err := filepath.Abs(sockPath)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// The socket may not exist yet at resolution time; hash the
		// cleaned absolute path (Clean, not EvalSymlinks) as the fallback
		// the capture's realpath canonicalization reduces to on plain paths.
		resolved = filepath.Clean(abs)
	}
	h := sha256Hex([]byte(resolved))
	return filepath.Join(claudeSessionsDir(home), fmt.Sprintf("%d.%s.key", pid, h)), nil
}

func readPeerToken(home string, pid int, sockPath string) (string, error) {
	p, err := peerKeyFile(home, pid, sockPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("peer key %s: %w", p, err)
	}
	var k struct {
		PeerToken string `json:"peerToken"`
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return "", fmt.Errorf("peer key %s: %w", p, err)
	}
	if !peerTokenRe.MatchString(k.PeerToken) {
		return "", fmt.Errorf("peer key %s: peerToken does not match the pinned shape", p)
	}
	return k.PeerToken, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}
