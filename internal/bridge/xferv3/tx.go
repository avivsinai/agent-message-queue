package xferv3

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"sort"
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
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
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
