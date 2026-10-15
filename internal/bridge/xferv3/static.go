package xferv3

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/flynn/noise"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	hkdfSalt = "amq-v3"
	hkdfInfo = "amq-v3-x25519-static"
)

type Static struct {
	priv [32]byte
	pub  [32]byte
}

func StaticFromSeed(seed []byte) (Static, error) {
	if len(seed) != ed25519.SeedSize {
		return Static{}, ErrLimit
	}
	reader := hkdf.New(sha256.New, seed, []byte(hkdfSalt), []byte(hkdfInfo))
	var priv [32]byte
	if _, err := io.ReadFull(reader, priv[:]); err != nil {
		return Static{}, fmt.Errorf("%w: static", err)
	}
	pub, err := noise.DH25519.DH(priv[:], curve25519.Basepoint[:])
	if err != nil {
		return Static{}, fmt.Errorf("%w: static public", err)
	}
	if len(pub) != 32 {
		return Static{}, ErrLimit
	}
	var static Static
	static.priv = priv
	copy(static.pub[:], pub)
	return static, nil
}

func (s Static) Public() [32]byte { return s.pub }
