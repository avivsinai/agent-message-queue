package linkio

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// link/<name>/grants.json holds, per binding of the link, its grant
// generation and the one grant accepted for it. A grant is valid only while
// its generation equals the record's: minting a new generation ends it.
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
}

// GrantRecord is one binding's generation and its stored grant, if any.
type GrantRecord struct {
	Generation string       `json:"generation"`
	Grant      *StoredGrant `json:"grant,omitempty"`
}

// Grants maps a binding name to its record.
type Grants map[string]GrantRecord

// ErrGrantGeneration is returned by StoreGrant when the binding's generation
// is no longer the one the grant was signed for.
var ErrGrantGeneration = errors.New("the binding's grant generation changed since the grant was signed")

// LoadGrants reads a link's grant records. No file is an empty map; an
// unreadable file is an error, so a caller deciding on a grant fails closed.
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
// a new generation and clears its stored grant. A mint only lowers
// authority, so it proceeds without an advisory file lock, and an
// unreadable file is replaced. A removed link has nothing to end.
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

// StoreGrant stores an accepted grant for its binding, replacing an older
// one. It refuses with ErrGrantGeneration unless the record's generation is
// the one the grant was signed for, read under the lock. Without an
// advisory file lock it refuses: storing a grant raises authority.
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
		rec.Grant = &sg
		g[sg.Binding] = rec
		return writeJSON(LinkDir(stateDir, link), grantsFile, g)
	})
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
}

// FileGrants is the GrantStore of link in stateDir: its grants.json.
func FileGrants(stateDir, link string) GrantStore { return fileGrants{stateDir: stateDir, link: link} }

type fileGrants struct{ stateDir, link string }

func (f fileGrants) Load() (Grants, error) { return LoadGrants(f.stateDir, f.link) }

func (f fileGrants) Generation(binding string) (string, error) {
	return EnsureGeneration(f.stateDir, f.link, binding)
}

func (f fileGrants) Store(sg StoredGrant) error { return StoreGrant(f.stateDir, f.link, sg) }

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
