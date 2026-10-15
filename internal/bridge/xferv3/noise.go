package xferv3

import (
	"fmt"
	"io"

	"github.com/flynn/noise"
)

const prologue = "amq-v3"

func handshake(from, to Static, rng io.Reader) (msg1, msg2 []byte, err error) {
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	initiator, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite,
		Random:      rng,
		Pattern:     noise.HandshakeIK,
		Initiator:   true,
		Prologue:    []byte(prologue),
		StaticKeypair: noise.DHKey{
			Private: from.priv[:],
			Public:  from.pub[:],
		},
		PeerStatic: to.pub[:],
	})
	if err != nil {
		return nil, nil, fmt.Errorf("xferv3: initiator handshake: %w", err)
	}
	responder, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite,
		Random:      rng,
		Pattern:     noise.HandshakeIK,
		Initiator:   false,
		Prologue:    []byte(prologue),
		StaticKeypair: noise.DHKey{
			Private: to.priv[:],
			Public:  to.pub[:],
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("xferv3: responder handshake: %w", err)
	}
	msg1, _, _, err = initiator.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("xferv3: handshake msg1: %w", err)
	}
	if _, _, _, err = responder.ReadMessage(nil, msg1); err != nil {
		return nil, nil, fmt.Errorf("xferv3: handshake read msg1: %w", err)
	}
	msg2, _, _, err = responder.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("xferv3: handshake msg2: %w", err)
	}
	if _, _, _, err = initiator.ReadMessage(nil, msg2); err != nil {
		return nil, nil, fmt.Errorf("xferv3: handshake read msg2: %w", err)
	}
	return msg1, msg2, nil
}
