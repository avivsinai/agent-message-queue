package linkio

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// link/<name>/grants.json holds, per binding of the link, its grant
// generation, the one grant accepted for it, and the issued_at of the newest
// grant accepted in that generation (its floor). A grant is valid only while
// its generation equals the record's, so minting a new generation ends it,
// and while the link's key list is the one it was accepted under.
const grantsFile = "grants.json"

// StoredGrant is an accepted session grant: what the owner signed, the
// digest of the signed bytes and the consent key that signed them.
type StoredGrant struct {
	GrantID         string `json:"grant_id"`
	Digest          string `json:"digest"`
	CredentialID    string `json:"credential_id"`
	Binding         string `json:"binding"`
	TargetID        string `json:"target_id"`
	Epoch           string `json:"epoch"`
	NativeSessionID string `json:"native_session_id"`
	Labels          Labels `json:"labels"`
	Generation      string `json:"grant_generation"`
	IssuedAt        string `json:"issued_at"`
	NotAfter        string `json:"not_after"`
	MinEvidence     string `json:"min_evidence"`
	// KeyList is the KeyList value of the link's consent keys that
	// verified the grant. The grant is live only while the link's keys
	// have this value, so a key added, dropped or revoked ends it.
	KeyList string `json:"key_list"`
}

// KeyList is the value of a link's key list: "sha256:" and the hex of its
// sorted consent credential ids joined by newlines.
func KeyList(credentialIDs []string) string {
	ids := slices.Sorted(slices.Values(credentialIDs))
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// GrantRecord is one binding's generation, its stored grant, if any, and
// its issued floor: the issued_at of the newest grant the store accepted
// for the binding in this generation. The floor outlives the grant, so a
// grant that ended, and every grant signed before it, is never stored
// again under the same generation.
type GrantRecord struct {
	Generation  string       `json:"generation"`
	Grant       *StoredGrant `json:"grant,omitempty"`
	IssuedFloor string       `json:"issued_floor,omitempty"`
}

// Grants maps a binding name to its record.
type Grants map[string]GrantRecord

// ErrGrantGeneration is returned by StoreGrant when the binding's generation
// is no longer the one the grant was signed for.
var ErrGrantGeneration = errors.New("the binding's grant generation changed since the grant was signed")

// ErrGrantNotNewer is returned by StoreGrant for a grant that was not issued
// after the binding's issued floor: a replaced or ended grant stays retired.
// StoreGrant wraps it with the floor.
var ErrGrantNotNewer = errors.New("the binding took a grant issued at")

// LoadGrants reads a link's grant records. No file is an empty map; an
// unreadable file is an error, so a caller deciding on a grant fails closed.
// A record with a stored grant and no issued floor reads with the grant's
// issued_at as its floor.
func LoadGrants(stateDir, link string) (Grants, error) {
	g := Grants{}
	err := readJSON(filepath.Join(LinkDir(stateDir, link), grantsFile), &g)
	if errors.Is(err, os.ErrNotExist) {
		return Grants{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s of link %q: %w", grantsFile, link, err)
	}
	if g == nil {
		g = Grants{}
	}
	for b, rec := range g {
		if rec.IssuedFloor == "" && rec.Grant != nil {
			rec.IssuedFloor = rec.Grant.IssuedAt
			g[b] = rec
		}
	}
	return g, nil
}

// EnsureGeneration returns the binding's generation, minting and writing a
// new one when the record or the file is missing or unreadable.
func EnsureGeneration(stateDir, link, binding string) (string, error) {
	var gen string
	err := withGrants(stateDir, link, func() error {
		g, err := LoadGrants(stateDir, link)
		if err == nil && g[binding].Generation != "" {
			gen = g[binding].Generation
			return nil
		}
		if err != nil {
			g = Grants{} // unreadable: every binding starts again with no grant
		}
		if gen, err = newGeneration(); err != nil {
			return err
		}
		g[binding] = GrantRecord{Generation: gen}
		return writeJSON(LinkDir(stateDir, link), grantsFile, g)
	})
	return gen, err
}

// MintGeneration gives each named binding, or with no names every record,
// a new generation and clears its stored grant and its issued floor: a
// grant names its generation, so nothing signed for the old one is stored
// again. A mint only lowers authority, so it proceeds without an advisory
// file lock, and an unreadable file is replaced. A removed link has nothing
// to end.
func MintGeneration(stateDir, link string, bindings ...string) error {
	err := withGrants(stateDir, link, func() error {
		g, err := LoadGrants(stateDir, link)
		if err != nil {
			g = Grants{}
		}
		if len(bindings) == 0 {
			if len(g) == 0 && err == nil {
				return nil
			}
			for b := range g {
				bindings = append(bindings, b)
			}
		}
		for _, b := range bindings {
			gen, err := newGeneration()
			if err != nil {
				return err
			}
			g[b] = GrantRecord{Generation: gen}
		}
		return writeJSON(LinkDir(stateDir, link), grantsFile, g)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ClearGrant removes the binding's stored grant when its digest is digest,
// and keeps the binding's generation and issued floor: the grant ended (it
// expired, or the link's key list is not the one it was accepted under) and
// stays ended, also after a restart, since StoreGrant refuses it. A
// clear only lowers authority, so it proceeds without an advisory file
// lock. A removed link has nothing to clear.
func ClearGrant(stateDir, link, binding, digest string) error {
	_, err := clearGrant(stateDir, link, binding, digest)
	return err
}

// RevokeGrant removes the binding's stored grant when its digest is digest,
// as ClearGrant does, and reports whether it removed it. The digest is
// compared and the grant cleared under the lock StoreGrant takes, so a
// grant stored meanwhile is never the one removed. Without an advisory file
// lock it refuses: the comparison must exclude a concurrent store.
func RevokeGrant(stateDir, link, binding, digest string) (bool, error) {
	if !lock.AdvisoryLockAvailable() {
		return false, errors.New("refusing to revoke a named grant without an advisory file lock")
	}
	return clearGrant(stateDir, link, binding, digest)
}

// clearGrant is the exact clear: it removes the binding's stored grant only
// when its digest is digest, keeps every other value of the file, and
// reports whether it changed the record.
func clearGrant(stateDir, link, binding, digest string) (bool, error) {
	changed := false
	err := withGrants(stateDir, link, func() error {
		g, err := LoadGrants(stateDir, link)
		if err != nil {
			return err
		}
		rec, ok := g[binding]
		if !ok || rec.Grant == nil || rec.Grant.Digest != digest {
			return nil
		}
		rec.Grant = nil
		g[binding] = rec
		if err := writeJSON(LinkDir(stateDir, link), grantsFile, g); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return changed, err
}

// StoreGrant stores an accepted grant for its binding, replacing an older
// one, and raises the binding's issued floor to its issued_at in the same
// write. It refuses with ErrGrantGeneration unless the record's generation
// is the one the grant was signed for, and with ErrGrantNotNewer unless the
// grant was issued after the floor, both read under the lock. The stored
// grant itself is a no-op. Without an advisory file lock it refuses:
// storing a grant raises authority.
func StoreGrant(stateDir, link string, sg StoredGrant) error {
	if !lock.AdvisoryLockAvailable() {
		return errors.New("refusing to store a grant without an advisory file lock")
	}
	return withGrants(stateDir, link, func() error {
		g, err := LoadGrants(stateDir, link)
		if err != nil {
			return err
		}
		rec, ok := g[sg.Binding]
		if !ok || rec.Generation == "" || rec.Generation != sg.Generation {
			return ErrGrantGeneration
		}
		if old := rec.Grant; old != nil && old.Digest == sg.Digest {
			return nil
		}
		if (rec.Grant != nil || rec.IssuedFloor != "") && !issuedAfter(sg.IssuedAt, rec.IssuedFloor) {
			return fmt.Errorf("%w %s", ErrGrantNotNewer, rec.IssuedFloor)
		}
		rec.Grant, rec.IssuedFloor = &sg, sg.IssuedAt
		g[sg.Binding] = rec
		return writeJSON(LinkDir(stateDir, link), grantsFile, g)
	})
}

// issuedAfter reports whether issued_at a is strictly later than b. A value
// that does not parse is never later.
func issuedAfter(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	return errA == nil && errB == nil && ta.After(tb)
}

// GrantStore is one link's grant records as the carrier reads and writes
// them.
type GrantStore interface {
	// Load reads every record (LoadGrants).
	Load() (Grants, error)
	// Generation returns the binding's generation, minting one when it has
	// none (EnsureGeneration).
	Generation(binding string) (string, error)
	// Store stores an accepted grant (StoreGrant).
	Store(StoredGrant) error
	// Mint gives the binding a new generation, which ends its grant, and
	// returns the new value (MintGeneration).
	Mint(binding string) (string, error)
	// Clear removes the binding's stored grant with that digest and keeps
	// its generation (ClearGrant).
	Clear(binding, digest string) error
	// Revoke removes the binding's stored grant with that digest under the
	// store's lock, keeps its generation and issued floor, and reports
	// whether it removed it (RevokeGrant).
	Revoke(binding, digest string) (bool, error)
}

// FileGrants is the GrantStore of link in stateDir: its grants.json.
func FileGrants(stateDir, link string) GrantStore { return fileGrants{stateDir: stateDir, link: link} }

type fileGrants struct{ stateDir, link string }

func (f fileGrants) Load() (Grants, error) { return LoadGrants(f.stateDir, f.link) }

func (f fileGrants) Generation(binding string) (string, error) {
	return EnsureGeneration(f.stateDir, f.link, binding)
}

func (f fileGrants) Store(sg StoredGrant) error { return StoreGrant(f.stateDir, f.link, sg) }

func (f fileGrants) Clear(binding, digest string) error {
	return ClearGrant(f.stateDir, f.link, binding, digest)
}

func (f fileGrants) Revoke(binding, digest string) (bool, error) {
	return RevokeGrant(f.stateDir, f.link, binding, digest)
}

func (f fileGrants) Mint(binding string) (string, error) {
	if err := MintGeneration(f.stateDir, f.link, binding); err != nil {
		return "", err
	}
	g, err := LoadGrants(f.stateDir, f.link)
	if err != nil {
		return "", err
	}
	gen := g[binding].Generation
	if gen == "" {
		return "", fmt.Errorf("link %q has no state directory", f.link)
	}
	return gen, nil
}

// withGrants runs fn under the lock that serializes a link's grant record
// changes, beside the file so the file can be replaced by rename. It never
// creates the link directory: a retired link stays deleted. Without an
// advisory file lock fn runs unlocked, and only on a link that exists.
func withGrants(stateDir, link string, fn func() error) error {
	dir := LinkDir(stateDir, link)
	if !lock.AdvisoryLockAvailable() {
		if _, err := os.Stat(dir); err != nil {
			return err
		}
		return fn()
	}
	return lock.WithExclusiveFileLock(filepath.Join(dir, grantsFile+".lock"), fn)
}

// newGeneration is "g_" and 26 base32 characters of 128 random bits.
func newGeneration() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "g_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)), nil
}
