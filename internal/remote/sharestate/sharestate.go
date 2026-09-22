// Package sharestate is the read-only runtime view of a shared session's
// enrolled body identity (bead 611.15). `amq-remote share` owns every write:
// minting the body key, the pending and staged windows, and publishing the
// enrolled generation. The relay client only reads the enrolled generation
// through this package; it never mints, renews, repairs or enrolls.
package sharestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// maxShareBytes bounds the share.json read; a real generation is five tags.
const maxShareBytes = 64 << 10

// ErrNotEnrolled means the session has no complete enrolled generation:
// pending or staged windows never authorize runtime traffic.
var ErrNotEnrolled = errors.New("session has no enrolled share generation")

// Credentials is one session's body key plus its enrolled owner-signed
// NIP-OA tags, each verified against this body and the one owner.
type Credentials struct {
	Session string
	Body    *bodykey.BodyKey
	Owner   string // 64-char lowercase hex
	tags    map[uint16]bodykey.AuthTag
}

// KeyDir returns <root>/extensions/remote/keys/<session>. The session must
// be one safe path component.
func KeyDir(root, session string) (string, error) {
	if session == "" || session == "." || session == ".." ||
		strings.ContainsAny(session, "/\\") || strings.ContainsRune(session, 0) {
		return "", fmt.Errorf("share session must be one path component, got %q", session)
	}
	return filepath.Join(root, "extensions", "remote", "keys", session), nil
}

// Load reads the session's body key and enrolled generation (share.json).
// Every tag must parse, name exactly one kind, and verify against this body
// key, and all tags must share one owner. Anything else is refused: a
// partial or inconsistent generation never authenticates.
func Load(root, session string) (*Credentials, error) {
	dir, err := KeyDir(root, session)
	if err != nil {
		return nil, err
	}
	body, err := bodykey.Load(filepath.Join(dir, "body.key"))
	if err != nil {
		return nil, fmt.Errorf("share %s: body key: %w", session, err)
	}
	raw, err := readRegular(filepath.Join(dir, "share.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("share %s: %w (run amq-remote share)", session, ErrNotEnrolled)
	}
	if err != nil {
		return nil, fmt.Errorf("share %s: %w", session, err)
	}
	var doc struct {
		Tags []struct {
			Kind        uint16 `json:"kind"`
			OwnerPubKey string `json:"owner_pubkey"`
			Conditions  string `json:"conditions"`
			Sig         string `json:"sig"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("share %s: share.json is invalid: %w", session, err)
	}
	if len(doc.Tags) == 0 {
		return nil, fmt.Errorf("share %s: %w (share.json carries no tags)", session, ErrNotEnrolled)
	}
	c := &Credentials{Session: session, Body: body, tags: map[uint16]bodykey.AuthTag{}}
	pub := body.PublicKeyHex()
	for _, ft := range doc.Tags {
		t, err := bodykey.ParseAuthTag([]string{"auth", ft.OwnerPubKey, ft.Conditions, ft.Sig})
		if err != nil {
			return nil, fmt.Errorf("share %s: kind %d tag: %w", session, ft.Kind, err)
		}
		if err := t.Verify(pub); err != nil {
			return nil, fmt.Errorf("share %s: kind %d tag does not verify for this body: %w", session, ft.Kind, err)
		}
		if c.Owner == "" {
			c.Owner = t.OwnerPubKey
		} else if c.Owner != t.OwnerPubKey {
			return nil, fmt.Errorf("share %s: tags are signed by more than one owner", session)
		}
		if _, dup := c.tags[ft.Kind]; dup {
			return nil, fmt.Errorf("share %s: two tags for kind %d", session, ft.Kind)
		}
		c.tags[ft.Kind] = *t
	}
	return c, nil
}

// TagFor returns the enrolled tag for kind if its signed conditions admit
// that kind at now. Expiry is AMQ policy on wall-clock time in addition to
// NIP-OA's signed-event-time rule, so an expired credential stops traffic
// even while an old socket stays open.
func (c *Credentials) TagFor(kind uint16, now time.Time) (bodykey.AuthTag, error) {
	t, ok := c.tags[kind]
	if !ok {
		return bodykey.AuthTag{}, fmt.Errorf("share %s: no enrolled tag for kind %d", c.Session, kind)
	}
	if err := t.Satisfies(kind, now.Unix()); err != nil {
		return bodykey.AuthTag{}, fmt.Errorf("share %s: kind %d tag: %w", c.Session, kind, err)
	}
	return t, nil
}

// readRegular reads a small regular file, refusing symlinks and other leaf
// types and anything over maxShareBytes.
func readRegular(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > maxShareBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxShareBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi2, err := f.Stat()
	if err != nil || !os.SameFile(fi, fi2) {
		return nil, fmt.Errorf("%s changed while opening", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxShareBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxShareBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxShareBytes)
	}
	return raw, nil
}
