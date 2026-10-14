package xferv3

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3/internal/canon"
)

const (
	recordTag      = "amq-xfer-v3"
	payloadSizeN   = 8
	payloadDigestN = 32
	recordFields   = 8
)

type Record struct {
	source Host
	handle Handle
	dest   Dest
	msg    MessageID
	thread ThreadID
	size   uint64
	sum    [32]byte
}

type ID struct{ sum [32]byte }

func NewRecord(source Host, handle Handle, dest Dest, message MessageID, thread ThreadID, payload []byte) (Record, error) {
	if source.s == "" || handle.s == "" || dest.host.s == "" || dest.agent.s == "" || message.s == "" || thread.s == "" {
		return Record{}, ErrName
	}
	if len(payload) == 0 || len(payload) > MaxPayloadBytes {
		return Record{}, ErrLimit
	}
	return Record{
		source: source,
		handle: handle,
		dest:   dest,
		msg:    message,
		thread: thread,
		size:   uint64(len(payload)),
		sum:    sha256.Sum256(payload),
	}, nil
}

func (r Record) ID() ID {
	raw, err := MarshalRecord(r)
	if err != nil {
		return ID{}
	}
	return ID{sum: sha256.Sum256(raw)}
}

func (r Record) PayloadSize() uint64  { return r.size }
func (r Record) PayloadSum() [32]byte { return r.sum }
func (r Record) Source() Host         { return r.source }
func (r Record) Handle() Handle       { return r.handle }
func (r Record) Dest() Dest           { return r.dest }
func (r Record) Message() MessageID   { return r.msg }
func (r Record) Thread() ThreadID     { return r.thread }

func (id ID) String() string {
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(id.sum[:]))
}

func (id ID) Sum() [32]byte { return id.sum }

func ParseID(s string) (ID, error) {
	if len(s) != IDTextLen || s != strings.ToLower(s) {
		return ID{}, ErrBadID
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(s))
	if err != nil || len(raw) != sha256.Size {
		return ID{}, ErrBadID
	}
	var id ID
	copy(id.sum[:], raw)
	if id.String() != s {
		return ID{}, ErrBadID
	}
	return id, nil
}

func MarshalRecord(r Record) ([]byte, error) {
	if r.source.s == "" || r.handle.s == "" || r.dest.host.s == "" || r.dest.agent.s == "" || r.msg.s == "" || r.thread.s == "" {
		return nil, ErrName
	}
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], r.size)
	raw := canon.AppendField(nil, []byte(recordTag))
	raw = canon.AppendField(raw, []byte(r.source.s))
	raw = canon.AppendField(raw, []byte(r.handle.s))
	raw = canon.AppendField(raw, []byte(r.dest.Alias()))
	raw = canon.AppendField(raw, []byte(r.msg.s))
	raw = canon.AppendField(raw, []byte(r.thread.s))
	raw = canon.AppendField(raw, size[:])
	raw = canon.AppendField(raw, r.sum[:])
	if len(raw) > MaxRecordBytes {
		return nil, ErrLimit
	}
	return raw, nil
}

func ParseRecord(raw []byte) (Record, error) {
	fields, err := canon.Fields(raw, recordFields, MaxRecordBytes)
	if err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrLimit, err)
	}
	if string(fields[0]) != recordTag {
		return Record{}, fmt.Errorf("%w: tag", ErrName)
	}
	if len(fields[6]) != payloadSizeN || len(fields[7]) != payloadDigestN {
		return Record{}, ErrLimit
	}
	source, err := ParseHost(string(fields[1]))
	if err != nil {
		return Record{}, err
	}
	handle, err := ParseHandle(string(fields[2]))
	if err != nil {
		return Record{}, err
	}
	dest, err := ParseDest(string(fields[3]))
	if err != nil {
		return Record{}, err
	}
	msg, err := ParseMessageID(string(fields[4]))
	if err != nil {
		return Record{}, err
	}
	thread, err := ParseThreadID(string(fields[5]))
	if err != nil {
		return Record{}, err
	}
	size := binary.BigEndian.Uint64(fields[6])
	if size == 0 || size > MaxPayloadBytes {
		return Record{}, ErrLimit
	}
	var sum [32]byte
	copy(sum[:], fields[7])
	rec := Record{
		source: source,
		handle: handle,
		dest:   dest,
		msg:    msg,
		thread: thread,
		size:   size,
		sum:    sum,
	}
	canonRec, err := MarshalRecord(rec)
	if err != nil {
		return Record{}, err
	}
	if string(canonRec) != string(raw) {
		return Record{}, ErrBadID
	}
	return rec, nil
}

type Sealed struct {
	rec     Record
	payload []byte
}

func Seal(rec Record, payload []byte) (Sealed, error) {
	if uint64(len(payload)) != rec.size {
		return Sealed{}, ErrPayload
	}
	if sha256.Sum256(payload) != rec.sum {
		return Sealed{}, ErrPayload
	}
	return Sealed{rec: rec, payload: append([]byte(nil), payload...)}, nil
}

func (s Sealed) Record() Record  { return s.rec }
func (s Sealed) Payload() []byte { return append([]byte(nil), s.payload...) }
