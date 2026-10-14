package xferv3

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/flynn/noise"
)

const (
	MaxFrameBytes          = MaxPayloadBytes + 64*1024
	MaxHandshakeBytes      = 256
	HandshakeDeadline      = 10 * time.Second
	RoundDeadline          = 2 * time.Minute
	frameOffer        byte = 1
	frameWant         byte = 2
	framePut          byte = 3
	frameStored       byte = 4
	frameNo           byte = 5
	frameDone         byte = 6
)

type Session struct {
	rw    io.ReadWriter
	send  *noise.CipherState
	recv  *noise.CipherState
	local Host
	peer  Host
}

func Dial(rw io.ReadWriter, local Static, peerPub [32]byte, rng io.Reader) (*Session, error) {
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite,
		Random:      rng,
		Pattern:     noise.HandshakeIK,
		Initiator:   true,
		Prologue:    []byte(prologue),
		StaticKeypair: noise.DHKey{
			Private: local.priv[:],
			Public:  local.pub[:],
		},
		PeerStatic: peerPub[:],
	})
	if err != nil {
		return nil, err
	}
	msg1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if err := writeRaw(rw, msg1, MaxHandshakeBytes); err != nil {
		return nil, err
	}
	msg2, err := readRaw(rw, MaxHandshakeBytes)
	if err != nil {
		return nil, err
	}
	_, cs0, cs1, err := hs.ReadMessage(nil, msg2)
	if err != nil {
		return nil, err
	}
	return &Session{rw: rw, send: cs0, recv: cs1}, nil
}

func Accept(rw io.ReadWriter, local Static, expectPub [32]byte, rng io.Reader) (*Session, error) {
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite,
		Random:      rng,
		Pattern:     noise.HandshakeIK,
		Initiator:   false,
		Prologue:    []byte(prologue),
		StaticKeypair: noise.DHKey{
			Private: local.priv[:],
			Public:  local.pub[:],
		},
	})
	if err != nil {
		return nil, err
	}
	msg1, err := readRaw(rw, MaxHandshakeBytes)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg1); err != nil {
		return nil, err
	}
	got := hs.PeerStatic()
	if len(got) != 32 {
		return nil, fmt.Errorf("xferv3: remote static is not the pinned key")
	}
	var gotPub [32]byte
	copy(gotPub[:], got)
	if gotPub != expectPub {
		return nil, fmt.Errorf("xferv3: remote static is not the pinned key")
	}
	msg2, cs0, cs1, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if err := writeRaw(rw, msg2, MaxHandshakeBytes); err != nil {
		return nil, err
	}
	return &Session{rw: rw, send: cs1, recv: cs0}, nil
}

func (s *Session) Send(kind byte, payload []byte) error {
	plain := append([]byte{kind}, payload...)
	cipher, err := s.send.Encrypt(nil, nil, plain)
	if err != nil {
		return err
	}
	return writeRaw(s.rw, cipher, MaxFrameBytes)
}

func (s *Session) Recv() (byte, []byte, error) {
	cipher, err := readRaw(s.rw, MaxFrameBytes)
	if err != nil {
		return 0, nil, err
	}
	plain, err := s.recv.Decrypt(nil, nil, cipher)
	if err != nil {
		return 0, nil, err
	}
	if len(plain) == 0 {
		return 0, nil, ErrLimit
	}
	return plain[0], plain[1:], nil
}

func writeRaw(w io.Writer, body []byte, max int) error {
	if len(body) == 0 || len(body) > max {
		return ErrLimit
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func readRaw(r io.Reader, max int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n <= 0 || n > max {
		return nil, ErrLimit
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
