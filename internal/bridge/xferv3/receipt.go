package xferv3

import (
	"crypto/ed25519"

	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3/internal/canon"
)

const (
	receiptTag    = "amq-receipt-v1"
	receiptStored = "STORED"
)

type Receipt struct {
	receiver Host
	sender   Host
	id       ID
}

type SignedReceipt struct {
	receipt Receipt
	sig     Signature
}

func NewReceipt(receiver, sender Host, id ID) (Receipt, error) {
	if receiver.s == "" || sender.s == "" || id == (ID{}) {
		return Receipt{}, ErrName
	}
	return Receipt{receiver: receiver, sender: sender, id: id}, nil
}

func SignReceipt(priv ed25519.PrivateKey, r Receipt) (SignedReceipt, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return SignedReceipt{}, ErrSignature
	}
	preimage, err := marshalReceipt(r)
	if err != nil {
		return SignedReceipt{}, err
	}
	var sig Signature
	copy(sig.raw[:], ed25519.Sign(priv, preimage))
	return SignedReceipt{receipt: r, sig: sig}, nil
}

func VerifyReceipt(pub ed25519.PublicKey, s SignedReceipt) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrSignature
	}
	preimage, err := marshalReceipt(s.receipt)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, preimage, s.sig.raw[:]) {
		return ErrSignature
	}
	return nil
}

func MarshalReceipt(r Receipt) ([]byte, error) {
	return marshalReceipt(r)
}

func (r Receipt) Receiver() Host { return r.receiver }
func (r Receipt) Sender() Host   { return r.sender }
func (r Receipt) ID() ID         { return r.id }

func (s SignedReceipt) Receipt() Receipt     { return s.receipt }
func (s SignedReceipt) Signature() Signature { return s.sig }

func marshalReceipt(r Receipt) ([]byte, error) {
	if r.receiver.s == "" || r.sender.s == "" || r.id == (ID{}) {
		return nil, ErrName
	}
	raw := canon.AppendField(nil, []byte(receiptTag))
	raw = canon.AppendField(raw, []byte(r.receiver.s))
	raw = canon.AppendField(raw, []byte(r.sender.s))
	raw = canon.AppendField(raw, []byte(r.id.String()))
	return canon.AppendField(raw, []byte(receiptStored)), nil
}
