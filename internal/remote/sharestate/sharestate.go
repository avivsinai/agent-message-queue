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
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// maxLeafBytes bounds each key-directory read; a real generation is a few
// tags and the key file is two lines.
const maxLeafBytes = 64 << 10

// maxWindow is the NIP-OA attestation bound share enforces (90 days); a
// generation claiming a longer window is not one share could have produced.
const maxWindow = 90 * 24 * time.Hour

// ErrNotEnrolled means the session has no complete enrolled generation:
// pending or staged windows never authorize runtime traffic.
var ErrNotEnrolled = errors.New("session has no enrolled share generation")

// Credentials is one session's body key plus its enrolled owner-signed
// NIP-OA tags, each verified against this body and the one owner.
type Credentials struct {
	Session  string
	Body     *bodykey.BodyKey
	Owner    string // 64-char lowercase hex
	NotAfter time.Time
	tags     map[uint16]bodykey.AuthTag
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

// Load reads the session's body key and enrolled generation (share.json)
// with the producer's rules (codex slice 1 review #3, #4):
//
//   - the directory chain from the root's extensions/ down to
//     keys/<session> is opened one component at a time through held
//     directory handles, each a real directory and never a symlink, and
//     both leaves are opened relative to the final handle; so no swap of a
//     parent, before or during the read, adopts an out-of-root key
//     (codex slice 1 review r2 #2);
//   - both leaves are opened no-follow and non-blocking and checked on the
//     opened handle, so a symlink or FIFO cannot be followed or hang
//     startup;
//   - the generation is complete and bounded: every base kind is present,
//     each tag's conditions are exactly the canonical string for its kind,
//     all tags share one not-after within the NIP-OA bound, each tag
//     verifies for this body, and one owner signed them all. A partial,
//     hand-edited or unbounded generation never authenticates.
func Load(root, session string) (*Credentials, error) {
	dir, err := KeyDir(root, session)
	if err != nil {
		return nil, err
	}
	d, err := openKeyDir(root, []string{"extensions", "remote", "keys", session})
	if err != nil {
		return nil, fmt.Errorf("share %s: key directory %s: %w", session, dir, err)
	}
	defer func() { _ = d.Close() }()
	keyRaw, keyInfo, err := readLeaf(d, "body.key")
	if err != nil {
		return nil, fmt.Errorf("share %s: body key: %w", session, err)
	}
	if keyInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("share %s: body key must be mode 0600 (has %o)", session, uint32(keyInfo.Mode().Perm()))
	}
	body, err := bodykey.Parse(keyRaw)
	if err != nil {
		return nil, fmt.Errorf("share %s: body key: %w", session, err)
	}
	raw, _, err := readLeaf(d, "share.json")
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
	var notAfter int64
	for i, ft := range doc.Tags {
		t, err := bodykey.ParseAuthTag([]string{"auth", ft.OwnerPubKey, ft.Conditions, ft.Sig})
		if err != nil {
			return nil, fmt.Errorf("share %s: kind %d tag: %w", session, ft.Kind, err)
		}
		na, ok := canonicalBound(ft.Kind, ft.Conditions)
		if !ok {
			return nil, fmt.Errorf("share %s: kind %d tag conditions %q are not the canonical share form", session, ft.Kind, ft.Conditions)
		}
		if i == 0 {
			notAfter = na
		} else if na != notAfter {
			return nil, fmt.Errorf("share %s: tags span more than one generation", session)
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
	for _, kind := range bodykey.ShareKinds {
		if _, ok := c.tags[kind]; !ok {
			return nil, fmt.Errorf("share %s: %w (generation lacks kind %d)", session, ErrNotEnrolled, kind)
		}
	}
	c.NotAfter = time.Unix(notAfter, 0)
	if time.Until(c.NotAfter) > maxWindow {
		return nil, fmt.Errorf("share %s: generation window exceeds the %d-day NIP-OA bound", session, int(maxWindow.Hours()/24))
	}
	return c, nil
}

// canonicalBound returns the not-after of a canonical share condition string
// for kind (bodykey.ShareConditions) and whether the string is exactly that.
func canonicalBound(kind uint16, conditions string) (int64, bool) {
	prefix := fmt.Sprintf("kind=%d&created_at<", kind)
	rest, ok := strings.CutPrefix(conditions, prefix)
	if !ok {
		return 0, false
	}
	na, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || na <= 0 {
		return 0, false
	}
	// The round trip rejects leading zeros, signs and any other spelling
	// share would not have produced.
	return na, bodykey.ShareConditions(kind, na) == conditions
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

// dirHandle is an opened, confined key directory; leaves open relative to
// it, never by path.
type dirHandle interface {
	openLeaf(name string) (*os.File, error)
	Close() error
}

// readLeaf reads a small regular file relative to d: no-follow,
// non-blocking open, regular-file check on the handle, bounded read.
func readLeaf(d dirHandle, name string) ([]byte, os.FileInfo, error) {
	f, err := d.openLeaf(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	if fi.Size() > maxLeafBytes {
		return nil, nil, fmt.Errorf("%s exceeds %d bytes", name, maxLeafBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxLeafBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > maxLeafBytes {
		return nil, nil, fmt.Errorf("%s exceeds %d bytes", name, maxLeafBytes)
	}
	return raw, fi, nil
}
