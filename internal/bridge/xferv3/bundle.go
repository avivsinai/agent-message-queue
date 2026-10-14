package xferv3

import (
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3/internal/canon"
)

const bundleFields = 3

func MarshalBundle(sealed Sealed, sig Signature) ([]byte, error) {
	rec, err := MarshalRecord(sealed.rec)
	if err != nil {
		return nil, err
	}
	if uint64(len(sealed.payload)) != sealed.rec.size {
		return nil, ErrPayload
	}
	raw := canon.AppendField(nil, rec)
	raw = canon.AppendField(raw, sig.raw[:])
	raw = canon.AppendField(raw, sealed.payload)
	return raw, nil
}

func ParseBundle(raw []byte) (Signed, []byte, error) {
	fields, err := canon.Fields(raw, bundleFields, MaxPayloadBytes)
	if err != nil {
		return Signed{}, nil, fmt.Errorf("%w: %v", ErrLimit, err)
	}
	rec, err := ParseRecord(fields[0])
	if err != nil {
		return Signed{}, nil, err
	}
	sig, err := parseSignature(fields[1])
	if err != nil {
		return Signed{}, nil, err
	}
	sealed, err := Seal(rec, fields[2])
	if err != nil {
		return Signed{}, nil, err
	}
	return Signed{rec: sealed.rec, sig: sig}, sealed.payload, nil
}
