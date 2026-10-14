package xferv3

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
)

func PutTx(root string, priv ed25519.PrivateKey, rec Record, payload []byte) (ID, error) {
	sealed, err := Seal(rec, payload)
	if err != nil {
		return ID{}, err
	}
	signed, err := Sign(priv, rec)
	if err != nil {
		return ID{}, err
	}
	if _, err := LoadPeer(root, rec.dest.host); err != nil {
		return ID{}, err
	}
	id := rec.ID()
	dir := filepath.Join(root, "bridge", txDirName, rec.dest.host.s)
	tmpDir := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return ID{}, fmt.Errorf("create tx directory: %w", err)
	}
	body, err := MarshalBundle(sealed, signed.sig)
	if err != nil {
		return ID{}, err
	}
	final := filepath.Join(dir, id.String())
	tmp, err := os.CreateTemp(tmpDir, "tx-*")
	if err != nil {
		return ID{}, err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return ID{}, err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return ID{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return ID{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return ID{}, err
	}
	if err := os.Link(tmpName, final); err != nil {
		if !os.IsExist(err) {
			_ = os.Remove(tmpName)
			return ID{}, err
		}
		existing, readErr := os.ReadFile(final)
		if readErr != nil {
			_ = os.Remove(tmpName)
			return ID{}, readErr
		}
		got, payloadGot, parseErr := ParseBundle(existing)
		if parseErr != nil || !sameRecord(got.rec, rec) || !bytes.Equal(payloadGot, payload) {
			_ = os.Remove(tmpName)
			return ID{}, ErrTxDiff
		}
		if bytes.Equal(got.sig.raw[:], signed.sig.raw[:]) {
			_ = os.Remove(tmpName)
			return id, nil
		}
		if err := os.Rename(tmpName, final); err != nil {
			_ = os.Remove(tmpName)
			return ID{}, err
		}
	} else {
		_ = os.Remove(tmpName)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return ID{}, err
	}
	defer func() { _ = dirFile.Close() }()
	if err := dirFile.Sync(); err != nil {
		return ID{}, err
	}
	return id, nil
}

func sameRecord(a, b Record) bool {
	left, err := MarshalRecord(a)
	if err != nil {
		return false
	}
	right, err := MarshalRecord(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}
