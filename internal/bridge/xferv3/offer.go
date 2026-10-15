package xferv3

import (
	"encoding/binary"
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3/internal/canon"
)

const (
	offerTag   = "amq-offer-v3"
	offerUintN = 4
)

type Offer struct {
	page    uint32
	entries []Signed
}

func NewOffer(page uint32, entries []Signed) (Offer, error) {
	if len(entries) == 0 || len(entries) > MaxOfferRecords {
		return Offer{}, ErrLimit
	}
	seen := make(map[ID]struct{}, len(entries))
	copied := make([]Signed, 0, len(entries))
	for _, entry := range entries {
		id := entry.ID()
		if id == (ID{}) {
			return Offer{}, ErrName
		}
		if _, ok := seen[id]; ok {
			return Offer{}, ErrBadID
		}
		seen[id] = struct{}{}
		copied = append(copied, entry)
	}
	offer := Offer{page: page, entries: copied}
	if _, err := MarshalOffer(offer); err != nil {
		return Offer{}, err
	}
	return offer, nil
}

func (o Offer) Page() uint32 { return o.page }

func (o Offer) Entries() []Signed {
	return append([]Signed(nil), o.entries...)
}

func MarshalOffer(o Offer) ([]byte, error) {
	if len(o.entries) == 0 || len(o.entries) > MaxOfferRecords {
		return nil, ErrLimit
	}
	var page, count [4]byte
	binary.BigEndian.PutUint32(page[:], o.page)
	binary.BigEndian.PutUint32(count[:], uint32(len(o.entries)))
	raw := canon.AppendField(nil, []byte(offerTag))
	raw = canon.AppendField(raw, page[:])
	raw = canon.AppendField(raw, count[:])
	seen := make(map[ID]struct{}, len(o.entries))
	for _, entry := range o.entries {
		rec, err := MarshalRecord(entry.rec)
		if err != nil {
			return nil, err
		}
		id := entry.ID()
		if _, ok := seen[id]; ok {
			return nil, ErrBadID
		}
		seen[id] = struct{}{}
		raw = canon.AppendField(raw, rec)
		raw = canon.AppendField(raw, entry.sig.raw[:])
	}
	if len(raw) > MaxOfferPageBytes {
		return nil, ErrLimit
	}
	return raw, nil
}

func ParseOffer(raw []byte) (Offer, error) {
	if len(raw) > MaxOfferPageBytes {
		return Offer{}, ErrLimit
	}
	tag, rest, err := canon.ReadField(raw, MaxOfferPageBytes)
	if err != nil {
		return Offer{}, fmt.Errorf("%w: %v", ErrLimit, err)
	}
	if string(tag) != offerTag {
		return Offer{}, fmt.Errorf("%w: tag", ErrName)
	}
	pageField, rest, err := canon.ReadField(rest, offerUintN)
	if err != nil || len(pageField) != offerUintN {
		return Offer{}, ErrLimit
	}
	countField, rest, err := canon.ReadField(rest, offerUintN)
	if err != nil || len(countField) != offerUintN {
		return Offer{}, ErrLimit
	}
	count := binary.BigEndian.Uint32(countField)
	if count == 0 || count > MaxOfferRecords {
		return Offer{}, ErrLimit
	}
	entries := make([]Signed, 0, count)
	seen := make(map[ID]struct{}, count)
	for i := uint32(0); i < count; i++ {
		recRaw, next, err := canon.ReadField(rest, MaxRecordBytes)
		if err != nil {
			return Offer{}, fmt.Errorf("%w: %v", ErrLimit, err)
		}
		sigRaw, next, err := canon.ReadField(next, signatureN)
		if err != nil {
			return Offer{}, fmt.Errorf("%w: %v", ErrLimit, err)
		}
		rec, err := ParseRecord(recRaw)
		if err != nil {
			return Offer{}, err
		}
		sig, err := parseSignature(sigRaw)
		if err != nil {
			return Offer{}, err
		}
		entry := Signed{rec: rec, sig: sig}
		id := entry.ID()
		if _, ok := seen[id]; ok {
			return Offer{}, ErrBadID
		}
		seen[id] = struct{}{}
		entries = append(entries, entry)
		rest = next
	}
	if len(rest) != 0 {
		return Offer{}, ErrLimit
	}
	return Offer{page: binary.BigEndian.Uint32(pageField), entries: entries}, nil
}
