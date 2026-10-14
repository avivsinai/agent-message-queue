package xferv3

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type Snapshot struct {
	Local     Host
	Peer      Host
	PeerKey   ed25519.PublicKey
	X25519    [32]byte
	AllowIn   []Handle
	PeerBytes uint64
	PeerFiles int
}

type Decision int

const (
	DecideWant Decision = iota
	DecideStored
	DecideNo
)

type Outcome struct {
	Decision Decision
	Reason   string
}

func Decide(snap Snapshot, signed Signed) Outcome {
	rec := signed.Record()
	id := rec.ID()
	if id == (ID{}) {
		return Outcome{Decision: DecideNo, Reason: ReasonBadID}
	}
	if rec.Source() != snap.Peer {
		return Outcome{Decision: DecideNo, Reason: ReasonAuth}
	}
	if err := Verify(snap.PeerKey, signed); err != nil {
		return Outcome{Decision: DecideNo, Reason: ReasonAuth}
	}
	if rec.Dest().Host() != snap.Local {
		return Outcome{Decision: DecideNo, Reason: ReasonDest}
	}
	if !allowHandle(snap.AllowIn, rec.Dest().Agent()) {
		return Outcome{Decision: DecideNo, Reason: ReasonACL}
	}
	if rec.PayloadSize() == 0 || rec.PayloadSize() > MaxPayloadBytes {
		return Outcome{Decision: DecideNo, Reason: ReasonSize}
	}
	return Outcome{Decision: DecideWant}
}

func Lookup(root string, snap Snapshot, id ID) (Outcome, error) {
	var out Outcome
	err := withIDLock(root, id, func() error {
		state, recErr := recoverLocked(root, snap, id)
		if recErr != nil {
			if errors.Is(recErr, ErrUncertain) {
				out = Outcome{Decision: DecideNo, Reason: ReasonUncertain}
				return nil
			}
			return recErr
		}
		if state == StatePublished {
			out = Outcome{Decision: DecideStored}
			return nil
		}
		out = Outcome{Decision: DecideWant}
		return nil
	})
	return out, err
}

func File(root string, snap Snapshot, signed Signed, payload []byte, localKey ed25519.PrivateKey) (SignedReceipt, error) {
	rec := signed.Record()
	id := rec.ID()
	var receipt SignedReceipt
	err := withIDLock(root, id, func() error {
		state, recErr := recoverLocked(root, snap, id)
		if recErr != nil {
			return recErr
		}
		if state == StatePublished {
			signedReceipt, signErr := signStored(snap.Local, snap.Peer, id, localKey)
			if signErr != nil {
				return signErr
			}
			receipt = signedReceipt
			return nil
		}
		got := Decide(snap, signed)
		if got.Decision != DecideWant {
			return fmt.Errorf("%w: %s", ErrNO, got.Reason)
		}
		sealed, err := Seal(rec, payload)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrNO, ReasonBadID)
		}
		if err := checkQuota(root, snap, rec); err != nil {
			return err
		}
		if err := fileLocked(root, snap, sealed, rec.Dest().Agent()); err != nil {
			return err
		}
		signedReceipt, err := signStored(snap.Local, snap.Peer, id, localKey)
		if err != nil {
			return err
		}
		receipt = signedReceipt
		return nil
	})
	return receipt, err
}

func Recover(root string, snap Snapshot, id ID) (string, error) {
	var state string
	err := withIDLock(root, id, func() error {
		got, recErr := recoverLocked(root, snap, id)
		state = got
		return recErr
	})
	return state, err
}

func signStored(local, peer Host, id ID, key ed25519.PrivateKey) (SignedReceipt, error) {
	r, err := NewReceipt(local, peer, id)
	if err != nil {
		return SignedReceipt{}, err
	}
	return SignReceipt(key, r)
}

func recoverLocked(root string, snap Snapshot, id ID) (string, error) {
	path := ledgerPath(root, id)
	facts, torn, err := readLedger(path)
	if err != nil {
		if errors.Is(err, ErrUncertain) {
			_ = appendFact(root, Fact{State: StateUncertain, Peer: snap.Peer, ID: id})
			return StateUncertain, err
		}
		return "", err
	}
	staged := fileExists(stagePath(root, snap.Peer, id))
	if torn {
		if staged {
			keep, keepErr := ledgerByteLen(facts)
			if keepErr != nil {
				return "", keepErr
			}
			if err := truncateTorn(path, keep); err != nil {
				return "", err
			}
		} else {
			if err := appendFact(root, Fact{State: StatePublished, Peer: snap.Peer, ID: id}); err != nil {
				return "", err
			}
			return StatePublished, nil
		}
	}
	state := foldState(facts)
	switch {
	case staged && state != StateIntent && state != StatePublished:
		if err := os.Remove(stagePath(root, snap.Peer, id)); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		return "", nil
	case state == StateIntent && staged:
		dest, destErr := destFromStage(stagePath(root, snap.Peer, id), snap)
		if destErr != nil {
			return "", destErr
		}
		if err := publishStaged(root, snap, id, dest); err != nil {
			return "", err
		}
		if err := appendFact(root, Fact{State: StatePublished, Peer: snap.Peer, ID: id}); err != nil {
			return "", err
		}
		return StatePublished, nil
	case state == StateIntent && !staged:
		if err := appendFact(root, Fact{State: StatePublished, Peer: snap.Peer, ID: id}); err != nil {
			return "", err
		}
		return StatePublished, nil
	default:
		return state, nil
	}
}

func fileLocked(root string, snap Snapshot, sealed Sealed, dest Handle) error {
	rec := sealed.Record()
	id := rec.ID()
	rxDir := filepath.Join(root, "bridge", "rx", snap.Peer.s)
	if err := os.MkdirAll(rxDir, 0o700); err != nil {
		return err
	}
	if err := syncDir(rxDir); err != nil {
		return err
	}
	agent := rec.Dest().Agent().String()
	if err := fsq.ValidateHandle(agent); err != nil {
		return fmt.Errorf("%w: %s", ErrNO, ReasonDest)
	}
	inboxNew := filepath.Join(root, "agents", agent, "inbox", "new")
	if _, err := os.Stat(inboxNew); err != nil {
		return fmt.Errorf("%w: %s", ErrNO, ReasonDest)
	}
	body, err := stampFrom(sealed.Payload(), snap.Peer, rec.Handle())
	if err != nil {
		return err
	}
	stage := stagePath(root, snap.Peer, id)
	if err := writeSyncedFile(stage, body); err != nil {
		return err
	}
	if err := writeSyncedFile(stage+".dest", []byte(dest.String())); err != nil {
		return err
	}
	if err := syncDir(rxDir); err != nil {
		return err
	}
	if err := appendFact(root, Fact{State: StateIntent, Peer: snap.Peer, ID: id}); err != nil {
		return err
	}
	if err := publishStaged(root, snap, id, dest); err != nil {
		return err
	}
	return appendFact(root, Fact{State: StatePublished, Peer: snap.Peer, ID: id})
}

func publishStaged(root string, snap Snapshot, id ID, dest Handle) error {
	stage := stagePath(root, snap.Peer, id)
	info, err := os.Lstat(stage)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("xferv3: staged copy is not a regular file")
	}
	agent := dest.String()
	if err := fsq.ValidateHandle(agent); err != nil {
		return fmt.Errorf("%w: %s", ErrNO, ReasonDest)
	}
	destPath := filepath.Join(root, "agents", agent, "inbox", "new", inboxName(id))
	if err := os.Rename(stage, destPath); err != nil {
		return err
	}
	_ = os.Remove(stage + ".dest")
	if err := syncDir(filepath.Dir(destPath)); err != nil {
		return err
	}
	rxDir := filepath.Join(root, "bridge", "rx", snap.Peer.s)
	if err := syncDir(rxDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func destFromStage(path string, snap Snapshot) (Handle, error) {
	if raw, err := os.ReadFile(path + ".dest"); err == nil {
		handle, parseErr := ParseHandle(strings.TrimSpace(string(raw)))
		if parseErr != nil || !allowHandle(snap.AllowIn, handle) {
			return Handle{}, fmt.Errorf("%w: %s", ErrNO, ReasonDest)
		}
		return handle, nil
	}
	if len(snap.AllowIn) == 1 {
		return snap.AllowIn[0], nil
	}
	return Handle{}, fmt.Errorf("%w: %s", ErrNO, ReasonDest)
}

func stampFrom(payload []byte, peer Host, handle Handle) ([]byte, error) {
	msg, err := format.ParseMessage(payload)
	if err != nil {
		return nil, err
	}
	msg.Header.From = peer.s + "/" + handle.s
	return msg.Marshal()
}

func allowHandle(allow []Handle, dest Handle) bool {
	for _, handle := range allow {
		if handle == dest {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func checkQuota(root string, snap Snapshot, rec Record) error {
	limitBytes := snap.PeerBytes
	if limitBytes == 0 {
		limitBytes = DefaultPeerBytes
	}
	limitFiles := snap.PeerFiles
	if limitFiles == 0 {
		limitFiles = DefaultPeerFiles
	}
	usedBytes, usedFiles, err := measureUnconsumed(root, snap)
	if err != nil {
		return err
	}
	if usedFiles+1 > limitFiles || usedBytes+rec.PayloadSize() > limitBytes {
		return fmt.Errorf("%w: %s", ErrBusy, ReasonBusy)
	}
	return nil
}

func measureUnconsumed(root string, snap Snapshot) (uint64, int, error) {
	var bytes uint64
	var files int
	rxDir := filepath.Join(root, "bridge", "rx", snap.Peer.s)
	entries, err := os.ReadDir(rxDir)
	if err != nil && !os.IsNotExist(err) {
		return 0, 0, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		bytes += uint64(info.Size())
		files++
	}
	seen := make(map[string]struct{})
	for _, handle := range snap.AllowIn {
		dir := filepath.Join(root, "agents", handle.s, "inbox", "new")
		inbox, readErr := os.ReadDir(dir)
		if readErr != nil {
			continue
		}
		for _, entry := range inbox {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".md") {
				continue
			}
			id, parseErr := ParseID(strings.TrimSuffix(name, ".md"))
			if parseErr != nil {
				continue
			}
			facts, torn, readErr := readLedger(ledgerPath(root, id))
			if readErr != nil || torn || len(facts) == 0 {
				continue
			}
			state := foldState(facts)
			if state != StateIntent && state != StatePublished {
				continue
			}
			if facts[len(facts)-1].Peer != snap.Peer {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			info, infoErr := entry.Info()
			if infoErr != nil {
				continue
			}
			bytes += uint64(info.Size())
			files++
		}
	}
	return bytes, files, nil
}
