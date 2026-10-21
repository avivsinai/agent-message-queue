package xferv3

import (
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type Owed struct {
	Signed  Signed
	Payload []byte
	Path    string
}

func ListTx(root string, peer Host) ([]Owed, error) {
	dir := filepath.Join(root, "bridge", txDirName, peer.s)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var owed []Owed
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "tmp" {
			continue
		}
		id, err := ParseID(name)
		if err != nil {
			continue
		}
		path := filepath.Join(dir, name)
		facts, torn, ledErr := readLedger(ledgerPath(root, id))
		if ledErr == nil && !torn && foldState(facts) == StateDiscard {
			continue
		}
		raw, ok, err := readBundle(path)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		signed, payload, err := ParseBundle(raw)
		if err != nil || signed.ID() != id {
			continue
		}
		owed = append(owed, Owed{Signed: signed, Payload: payload, Path: path})
	}
	sort.Slice(owed, func(i, j int) bool {
		return owed[i].Signed.ID().String() < owed[j].Signed.ID().String()
	})
	return owed, nil
}

// maxBundleBytes is the largest bundle ParseBundle can accept: three
// length-prefixed fields holding a record, a signature and a payload.
const maxBundleBytes = 3*4 + MaxRecordBytes + ed25519.SignatureSize + MaxPayloadBytes

// readBundle reads a tx bundle without following a symlink at its name. ok
// is false for a missing, non-regular or oversized file, which holds no
// valid bundle.
func readBundle(path string) (raw []byte, ok bool, err error) {
	file, _, err := fsq.OpenRegularNoFollowBounded(path, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, fsq.ErrNotRegularFile) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	raw, err = io.ReadAll(io.LimitReader(file, maxBundleBytes+1))
	if err != nil {
		return nil, false, err
	}
	return raw, len(raw) <= maxBundleBytes, nil
}

func ConsumeTx(path string) error {
	return os.Remove(path)
}

func VerifyAndConsume(path string, pub ed25519.PublicKey, receipt SignedReceipt, receiver, sender Host, id ID) error {
	got := receipt.Receipt()
	if got.ID() != id || got.Receiver() != receiver || got.Sender() != sender {
		return ErrBadID
	}
	if err := VerifyReceipt(pub, receipt); err != nil {
		return err
	}
	return ConsumeTx(path)
}
