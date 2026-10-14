package xferv3

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3/internal/canon"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

const (
	factTag          = "amq-fact-v3"
	factFields       = 4
	maxFactBytes     = 256
	ledgerPrefixN    = 2
	StateIntent      = "intent"
	StatePublished   = "published"
	StateUncertain   = "uncertain"
	StateDiscard     = "discard"
	ReasonACL        = "acl"
	ReasonAuth       = "auth"
	ReasonBadID      = "bad-id"
	ReasonBusy       = "busy"
	ReasonDest       = "dest"
	ReasonSize       = "size"
	ReasonUncertain  = "uncertain"
	DefaultPeerBytes = 64 << 20
	DefaultPeerFiles = 4096
)

var (
	ErrNO        = fmt.Errorf("xferv3: refused")
	ErrUncertain = fmt.Errorf("xferv3: uncertain")
	ErrBusy      = fmt.Errorf("xferv3: busy")
)

type Fact struct {
	State string
	Peer  Host
	ID    ID
}

func ledgerPath(root string, id ID) string {
	text := id.String()
	return filepath.Join(root, "bridge", "ledger", "v3", text[:ledgerPrefixN], text)
}

func lockPath(root string, id ID) string {
	return ledgerPath(root, id) + ".lock"
}

func stagePath(root string, peer Host, id ID) string {
	return filepath.Join(root, "bridge", "rx", peer.s, id.String())
}

func inboxName(id ID) string {
	return id.String() + ".md"
}

func withIDLock(root string, id ID, fn func() error) error {
	dir := filepath.Dir(lockPath(root, id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(lockPath(root, id), fn)
}

func marshalFact(f Fact) ([]byte, error) {
	if f.State == "" || f.Peer.s == "" || f.ID == (ID{}) {
		return nil, ErrName
	}
	raw := canon.AppendField(nil, []byte(factTag))
	raw = canon.AppendField(raw, []byte(f.State))
	raw = canon.AppendField(raw, []byte(f.Peer.s))
	raw = canon.AppendField(raw, []byte(f.ID.String()))
	return raw, nil
}

func parseFact(raw []byte) (Fact, error) {
	fields, err := canon.Fields(raw, factFields, maxFactBytes)
	if err != nil {
		return Fact{}, err
	}
	if string(fields[0]) != factTag {
		return Fact{}, ErrName
	}
	state := string(fields[1])
	switch state {
	case StateIntent, StatePublished, StateUncertain, StateDiscard:
	default:
		return Fact{}, ErrName
	}
	peer, err := ParseHost(string(fields[2]))
	if err != nil {
		return Fact{}, err
	}
	id, err := ParseID(string(fields[3]))
	if err != nil {
		return Fact{}, err
	}
	return Fact{State: state, Peer: peer, ID: id}, nil
}

func encodeLedgerRecord(fact Fact) ([]byte, error) {
	body, err := marshalFact(fact)
	if err != nil {
		return nil, err
	}
	sum := crc32.ChecksumIEEE(body)
	out := make([]byte, 4+len(body)+4)
	binary.BigEndian.PutUint32(out[:4], uint32(len(body)))
	copy(out[4:4+len(body)], body)
	binary.BigEndian.PutUint32(out[4+len(body):], sum)
	return out, nil
}

func readLedger(path string) (facts []Fact, torn bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, false, err
	}
	off := 0
	for off < len(raw) {
		if len(raw)-off < 8 {
			return facts, true, nil
		}
		n := int(binary.BigEndian.Uint32(raw[off : off+4]))
		if n <= 0 || n > maxFactBytes {
			return facts, true, nil
		}
		if len(raw)-off < 8+n {
			return facts, true, nil
		}
		body := raw[off+4 : off+4+n]
		sum := binary.BigEndian.Uint32(raw[off+4+n : off+8+n])
		if crc32.ChecksumIEEE(body) != sum {
			if off+8+n == len(raw) {
				return facts, true, nil
			}
			return facts, false, fmt.Errorf("%w: checksum", ErrUncertain)
		}
		fact, parseErr := parseFact(body)
		if parseErr != nil {
			if off+8+n == len(raw) {
				return facts, true, nil
			}
			return facts, false, fmt.Errorf("%w: %v", ErrUncertain, parseErr)
		}
		facts = append(facts, fact)
		off += 8 + n
	}
	return facts, false, nil
}

func foldState(facts []Fact) string {
	state := ""
	for _, fact := range facts {
		state = fact.State
	}
	return state
}

func appendFact(root string, fact Fact) error {
	path := ledgerPath(root, fact.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	rec, err := encodeLedgerRecord(fact)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(rec); err != nil {
		_ = file.Close()
		return err
	}
	if err := syncFile(file); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func truncateTorn(path string, keep int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := file.Truncate(keep); err != nil {
		_ = file.Close()
		return err
	}
	if err := syncFile(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func ledgerByteLen(facts []Fact) (int64, error) {
	var n int64
	for _, fact := range facts {
		raw, err := encodeLedgerRecord(fact)
		if err != nil {
			return 0, err
		}
		n += int64(len(raw))
	}
	return n, nil
}
