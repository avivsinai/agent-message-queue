package xferv3

import (
	"crypto/ed25519"
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3/internal/canon"
)

const (
	envTag     = "amq-env-v3"
	signatureN = 64
)

type Signature struct{ raw [64]byte }

type Signed struct {
	rec Record
	sig Signature
}

func Sign(priv ed25519.PrivateKey, rec Record) (Signed, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return Signed{}, ErrSignature
	}
	id := rec.ID()
	if id == (ID{}) {
		return Signed{}, ErrName
	}
	var sig Signature
	copy(sig.raw[:], ed25519.Sign(priv, signaturePreimage(id)))
	return Signed{rec: rec, sig: sig}, nil
}

func Verify(pub ed25519.PublicKey, signed Signed) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrSignature
	}
	id := signed.rec.ID()
	if id == (ID{}) {
		return ErrName
	}
	if !ed25519.Verify(pub, signaturePreimage(id), signed.sig.raw[:]) {
		return ErrSignature
	}
	return nil
}

func (s Signed) Record() Record       { return s.rec }
func (s Signed) Signature() Signature { return s.sig }
func (s Signed) ID() ID               { return s.rec.ID() }
func (s Signature) Bytes() []byte     { return append([]byte(nil), s.raw[:]...) }

func signaturePreimage(id ID) []byte {
	raw := canon.AppendField(nil, []byte(envTag))
	return canon.AppendField(raw, []byte(id.String()))
}

func parseSignature(raw []byte) (Signature, error) {
	if len(raw) != signatureN {
		return Signature{}, fmt.Errorf("%w: length", ErrSignature)
	}
	var sig Signature
	copy(sig.raw[:], raw)
	return sig, nil
}
