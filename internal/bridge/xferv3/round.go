package xferv3

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3/internal/canon"
)

type HostState struct {
	Root     string
	Local    Host
	LocalKey ed25519.PrivateKey
	Static   Static
	Snap     Snapshot
}

func Round(sess *Session, self HostState, initiator bool) error {
	selfDone := false
	peerDone := false
	for !selfDone || !peerDone {
		if initiator {
			if !selfDone {
				done, err := offerPage(sess, self)
				if err != nil {
					return err
				}
				selfDone = done
			}
			if !peerDone {
				done, err := answerPage(sess, self)
				if err != nil {
					return err
				}
				peerDone = done
			}
		} else {
			if !peerDone {
				done, err := answerPage(sess, self)
				if err != nil {
					return err
				}
				peerDone = done
			}
			if !selfDone {
				done, err := offerPage(sess, self)
				if err != nil {
					return err
				}
				selfDone = done
			}
		}
	}
	return nil
}

func offerPage(sess *Session, self HostState) (bool, error) {
	owed, err := ListTx(self.Root, self.Snap.Peer)
	if err != nil {
		return false, err
	}
	start := loadCursor(self.Root, self.Snap.Peer)
	page := takePage(owed, start)
	if len(page) == 0 {
		if err := sess.Send(frameDone, nil); err != nil {
			return false, err
		}
		return true, nil
	}
	entries := make([]Signed, 0, len(page))
	for _, item := range page {
		entries = append(entries, item.Signed)
	}
	offer, err := NewOffer(0, entries)
	if err != nil {
		return false, err
	}
	raw, err := MarshalOffer(offer)
	if err != nil {
		return false, err
	}
	if err := sess.Send(frameOffer, raw); err != nil {
		return false, err
	}
	byID := map[ID]Owed{}
	for _, item := range page {
		byID[item.Signed.ID()] = item
	}
	for range page {
		kind, payload, err := sess.Recv()
		if err != nil {
			return false, err
		}
		switch kind {
		case frameStored:
			receipt, err := parseSignedReceipt(payload)
			if err != nil {
				return false, err
			}
			item, ok := byID[receipt.Receipt().ID()]
			if !ok {
				return false, ErrBadID
			}
			if err := VerifyAndConsume(item.Path, self.Snap.PeerKey, receipt, self.Snap.Peer, self.Snap.Local, item.Signed.ID()); err != nil {
				return false, err
			}
		case frameWant:
			id, err := ParseID(string(payload))
			if err != nil {
				return false, err
			}
			item, ok := byID[id]
			if !ok {
				return false, ErrBadID
			}
			bundle, err := MarshalBundle(mustSeal(item), item.Signed.Signature())
			if err != nil {
				return false, err
			}
			if err := sess.Send(framePut, bundle); err != nil {
				return false, err
			}
			kind, payload, err := sess.Recv()
			if err != nil {
				return false, err
			}
			if kind != frameStored {
				if kind == frameNo {
					_ = writeNo(item.Path, payload)
					continue
				}
				return false, fmt.Errorf("xferv3: expected STORED after PUT")
			}
			receipt, err := parseSignedReceipt(payload)
			if err != nil {
				return false, err
			}
			if err := VerifyAndConsume(item.Path, self.Snap.PeerKey, receipt, self.Snap.Peer, self.Snap.Local, item.Signed.ID()); err != nil {
				return false, err
			}
		case frameNo:
			id, _, err := parseNo(payload)
			if err != nil {
				return false, err
			}
			item, ok := byID[id]
			if !ok {
				return false, ErrBadID
			}
			_ = writeNo(item.Path, payload)
		default:
			return false, fmt.Errorf("xferv3: unexpected answer frame %d", kind)
		}
	}
	saveCursor(self.Root, self.Snap.Peer, page[len(page)-1].Signed.ID())
	return false, nil
}

func answerPage(sess *Session, self HostState) (bool, error) {
	kind, payload, err := sess.Recv()
	if err != nil {
		return false, err
	}
	if kind == frameDone {
		return true, nil
	}
	if kind != frameOffer {
		return false, fmt.Errorf("xferv3: expected OFFER")
	}
	offer, err := ParseOffer(payload)
	if err != nil {
		return false, err
	}
	for _, entry := range offer.Entries() {
		if err := answerOne(sess, self, entry); err != nil {
			return false, err
		}
	}
	return false, nil
}

func answerOne(sess *Session, self HostState, entry Signed) error {
	look, err := Lookup(self.Root, self.Snap, entry.ID())
	if err != nil {
		return err
	}
	if look.Decision == DecideStored {
		return sendStored(sess, self, entry.ID())
	}
	if look.Decision == DecideNo {
		return sendNo(sess, entry.ID(), look.Reason)
	}
	got := Decide(self.Snap, entry)
	if got.Decision == DecideNo {
		return sendNo(sess, entry.ID(), got.Reason)
	}
	if err := sess.Send(frameWant, []byte(entry.ID().String())); err != nil {
		return err
	}
	kind, payload, err := sess.Recv()
	if err != nil {
		return err
	}
	if kind != framePut {
		return fmt.Errorf("xferv3: expected PUT")
	}
	signed, body, err := ParseBundle(payload)
	if err != nil {
		return sendNo(sess, entry.ID(), ReasonBadID)
	}
	if signed.ID() != entry.ID() {
		return sendNo(sess, entry.ID(), ReasonBadID)
	}
	receipt, err := File(self.Root, self.Snap, signed, body, self.LocalKey)
	if err != nil {
		if errors.Is(err, ErrNO) || errors.Is(err, ErrBusy) {
			return sendNo(sess, entry.ID(), noReason(err))
		}
		return err
	}
	return sendSignedReceipt(sess, receipt)
}

func sendStored(sess *Session, self HostState, id ID) error {
	receipt, err := signStored(self.Local, self.Snap.Peer, id, self.LocalKey)
	if err != nil {
		return err
	}
	return sendSignedReceipt(sess, receipt)
}

func sendSignedReceipt(sess *Session, receipt SignedReceipt) error {
	raw, err := marshalSignedReceipt(receipt)
	if err != nil {
		return err
	}
	return sess.Send(frameStored, raw)
}

func sendNo(sess *Session, id ID, reason string) error {
	raw := canon.AppendField(nil, []byte(id.String()))
	raw = canon.AppendField(raw, []byte(reason))
	return sess.Send(frameNo, raw)
}

func parseNo(raw []byte) (ID, string, error) {
	fields, err := canon.Fields(raw, 2, 64)
	if err != nil {
		return ID{}, "", err
	}
	id, err := ParseID(string(fields[0]))
	if err != nil {
		return ID{}, "", err
	}
	return id, string(fields[1]), nil
}

func marshalSignedReceipt(s SignedReceipt) ([]byte, error) {
	body, err := MarshalReceipt(s.Receipt())
	if err != nil {
		return nil, err
	}
	raw := canon.AppendField(nil, body)
	return canon.AppendField(raw, s.Signature().Bytes()), nil
}

func parseSignedReceipt(raw []byte) (SignedReceipt, error) {
	fields, err := canon.Fields(raw, 2, MaxRecordBytes)
	if err != nil {
		return SignedReceipt{}, err
	}
	recFields, err := canon.Fields(fields[0], 5, MaxRecordBytes)
	if err != nil {
		return SignedReceipt{}, err
	}
	if string(recFields[0]) != receiptTag {
		return SignedReceipt{}, ErrName
	}
	receiver, err := ParseHost(string(recFields[1]))
	if err != nil {
		return SignedReceipt{}, err
	}
	sender, err := ParseHost(string(recFields[2]))
	if err != nil {
		return SignedReceipt{}, err
	}
	id, err := ParseID(string(recFields[3]))
	if err != nil {
		return SignedReceipt{}, err
	}
	if string(recFields[4]) != receiptStored {
		return SignedReceipt{}, ErrName
	}
	sig, err := parseSignature(fields[1])
	if err != nil {
		return SignedReceipt{}, err
	}
	receipt, err := NewReceipt(receiver, sender, id)
	if err != nil {
		return SignedReceipt{}, err
	}
	return SignedReceipt{receipt: receipt, sig: sig}, nil
}

func takePage(owed []Owed, after string) []Owed {
	var out []Owed
	for _, item := range owed {
		if after != "" && item.Signed.ID().String() <= after {
			continue
		}
		out = append(out, item)
		if len(out) == MaxOfferRecords {
			break
		}
	}
	if after != "" && len(out) == 0 {
		n := MaxOfferRecords
		if len(owed) < n {
			n = len(owed)
		}
		return owed[:n]
	}
	return out
}

func cursorPath(root string, peer Host) string {
	return filepath.Join(root, "bridge", "sync", peer.s+".cursor")
}

func loadCursor(root string, peer Host) string {
	raw, err := os.ReadFile(cursorPath(root, peer))
	if err != nil {
		return ""
	}
	return string(raw)
}

func saveCursor(root string, peer Host, id ID) {
	dir := filepath.Join(root, "bridge", "sync")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(cursorPath(root, peer), []byte(id.String()), 0o600)
}

func writeNo(path string, payload []byte) error {
	return os.WriteFile(path+".no", payload, 0o600)
}

func mustSeal(item Owed) Sealed {
	sealed, err := Seal(item.Signed.Record(), item.Payload)
	if err != nil {
		return Sealed{}
	}
	return sealed
}

func noReason(err error) string {
	text := err.Error()
	for _, reason := range []string{ReasonACL, ReasonAuth, ReasonBadID, ReasonBusy, ReasonDest, ReasonSize, ReasonUncertain} {
		if strings.Contains(text, reason) {
			return reason
		}
	}
	return ReasonBusy
}

func SyncPipe(left, right io.ReadWriter, a, b HostState) error {
	errc := make(chan error, 2)
	go func() {
		sess, err := Dial(left, a.Static, b.Static.Public(), rand.Reader)
		if err != nil {
			errc <- err
			return
		}
		errc <- Round(sess, a, true)
	}()
	go func() {
		sess, err := Accept(right, b.Static, a.Static.Public(), rand.Reader)
		if err != nil {
			errc <- err
			return
		}
		errc <- Round(sess, b, false)
	}()
	if err := <-errc; err != nil {
		return err
	}
	return <-errc
}
