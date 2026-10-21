package xferv3

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ID{}, fmt.Errorf("create tx directory: %w", err)
	}
	body, err := MarshalBundle(sealed, signed.sig)
	if err != nil {
		return ID{}, err
	}
	final := filepath.Join(dir, id.String())
	created, err := fsq.CreateNewFile(final, body, 0o600)
	if err != nil {
		return ID{}, err
	}
	if created {
		return id, nil
	}
	existing, err := fsq.ReadRegularNoFollowBounded(final, maxBundleBytes)
	if err != nil {
		return ID{}, err
	}
	got, payloadGot, parseErr := ParseBundle(existing)
	if parseErr != nil || !sameRecord(got.rec, rec) || !bytes.Equal(payloadGot, payload) {
		return ID{}, ErrTxDiff
	}
	if bytes.Equal(got.sig.raw[:], signed.sig.raw[:]) {
		return id, nil
	}
	if _, err := fsq.WriteFileAtomic(dir, id.String(), body, 0o600); err != nil {
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
