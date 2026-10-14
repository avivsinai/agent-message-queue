package xferv3

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type vectorFile struct {
	Record struct {
		SourceHost      string `json:"source_host"`
		Handle          string `json:"handle"`
		Dest            string `json:"dest"`
		SourceMessageID string `json:"source_message_id"`
		ThreadID        string `json:"thread_id"`
		PayloadHex      string `json:"payload_hex"`
		RecordHex       string `json:"record_hex"`
		ID              string `json:"id"`
	} `json:"record"`
	Signature struct {
		SeedHex      string `json:"seed_hex"`
		PreimageHex  string `json:"preimage_hex"`
		SignatureHex string `json:"signature_hex"`
	} `json:"signature"`
	Receipt struct {
		Receiver     string `json:"receiver"`
		Sender       string `json:"sender"`
		PreimageHex  string `json:"preimage_hex"`
		SignatureHex string `json:"signature_hex"`
	} `json:"receipt"`
	Offer struct {
		Page    uint32 `json:"page"`
		PageHex string `json:"page_hex"`
	} `json:"offer"`
	Bundle struct {
		BundleHex string `json:"bundle_hex"`
	} `json:"bundle"`
	Static struct {
		SeedHex    string `json:"seed_hex"`
		PrivateHex string `json:"private_hex"`
		PublicHex  string `json:"public_hex"`
	} `json:"static"`
	Handshake struct {
		InitiatorSeedHex string `json:"initiator_seed_hex"`
		ResponderSeedHex string `json:"responder_seed_hex"`
		EphemeralHex     string `json:"ephemeral_hex"`
		Message1Hex      string `json:"message1_hex"`
		Message2Hex      string `json:"message2_hex"`
	} `json:"handshake"`
}

func TestVectors(t *testing.T) {
	doc := loadVectors(t)

	payload := mustHex(t, "payload", doc.Record.PayloadHex)
	source, err := ParseHost(doc.Record.SourceHost)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := ParseHandle(doc.Record.Handle)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := ParseDest(doc.Record.Dest)
	if err != nil {
		t.Fatal(err)
	}
	message, err := ParseMessageID(doc.Record.SourceMessageID)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := ParseThreadID(doc.Record.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecord(source, handle, dest, message, thread, payload)
	if err != nil {
		t.Fatal(err)
	}
	gotRecord, err := MarshalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	wantRecord := mustHex(t, "record", doc.Record.RecordHex)
	if !bytes.Equal(gotRecord, wantRecord) {
		t.Fatalf("MarshalRecord = %x, want %x", gotRecord, wantRecord)
	}
	if rec.ID().String() != doc.Record.ID {
		t.Fatalf("ID = %s, want %s", rec.ID().String(), doc.Record.ID)
	}
	parsed, err := ParseRecord(wantRecord)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID().String() != doc.Record.ID {
		t.Fatalf("ParseRecord id = %s, want %s", parsed.ID().String(), doc.Record.ID)
	}

	seed := mustHex(t, "seed", doc.Signature.SeedHex)
	priv := ed25519.NewKeyFromSeed(seed)
	signed, err := Sign(priv, rec)
	if err != nil {
		t.Fatal(err)
	}
	wantPreimage := mustHex(t, "sig preimage", doc.Signature.PreimageHex)
	if !bytes.Equal(signaturePreimage(rec.ID()), wantPreimage) {
		t.Fatalf("signature preimage = %x, want %x", signaturePreimage(rec.ID()), wantPreimage)
	}
	wantSig := mustHex(t, "signature", doc.Signature.SignatureHex)
	if !bytes.Equal(signed.Signature().Bytes(), wantSig) {
		t.Fatalf("Sign = %x, want %x", signed.Signature().Bytes(), wantSig)
	}
	if err := Verify(priv.Public().(ed25519.PublicKey), signed); err != nil {
		t.Fatal(err)
	}

	receiver, err := ParseHost(doc.Receipt.Receiver)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := ParseHost(doc.Receipt.Sender)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewReceipt(receiver, sender, rec.ID())
	if err != nil {
		t.Fatal(err)
	}
	gotReceipt, err := MarshalReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	wantReceipt := mustHex(t, "receipt", doc.Receipt.PreimageHex)
	if !bytes.Equal(gotReceipt, wantReceipt) {
		t.Fatalf("MarshalReceipt = %x, want %x", gotReceipt, wantReceipt)
	}
	signedReceipt, err := SignReceipt(priv, receipt)
	if err != nil {
		t.Fatal(err)
	}
	wantReceiptSig := mustHex(t, "receipt sig", doc.Receipt.SignatureHex)
	if !bytes.Equal(signedReceipt.Signature().Bytes(), wantReceiptSig) {
		t.Fatalf("SignReceipt = %x, want %x", signedReceipt.Signature().Bytes(), wantReceiptSig)
	}
	if err := VerifyReceipt(priv.Public().(ed25519.PublicKey), signedReceipt); err != nil {
		t.Fatal(err)
	}

	offer, err := NewOffer(doc.Offer.Page, []Signed{signed})
	if err != nil {
		t.Fatal(err)
	}
	gotOffer, err := MarshalOffer(offer)
	if err != nil {
		t.Fatal(err)
	}
	wantOffer := mustHex(t, "offer", doc.Offer.PageHex)
	if !bytes.Equal(gotOffer, wantOffer) {
		t.Fatalf("MarshalOffer = %x, want %x", gotOffer, wantOffer)
	}

	sealed, err := Seal(rec, payload)
	if err != nil {
		t.Fatal(err)
	}
	gotBundle, err := MarshalBundle(sealed, signed.Signature())
	if err != nil {
		t.Fatal(err)
	}
	wantBundle := mustHex(t, "bundle", doc.Bundle.BundleHex)
	if !bytes.Equal(gotBundle, wantBundle) {
		t.Fatalf("MarshalBundle = %x, want %x", gotBundle, wantBundle)
	}

	staticSeed := mustHex(t, "static seed", doc.Static.SeedHex)
	staticKey, err := StaticFromSeed(staticSeed)
	if err != nil {
		t.Fatal(err)
	}
	wantPriv := mustHex(t, "static private", doc.Static.PrivateHex)
	if !bytes.Equal(staticKey.priv[:], wantPriv) {
		t.Fatalf("Static private = %x, want %x", staticKey.priv[:], wantPriv)
	}
	wantPub := mustHex(t, "static public", doc.Static.PublicHex)
	pub := staticKey.Public()
	if !bytes.Equal(pub[:], wantPub) {
		t.Fatalf("Static public = %x, want %x", pub[:], wantPub)
	}

	fromSeed := mustHex(t, "initiator seed", doc.Handshake.InitiatorSeedHex)
	toSeed := mustHex(t, "responder seed", doc.Handshake.ResponderSeedHex)
	from, err := StaticFromSeed(fromSeed)
	if err != nil {
		t.Fatal(err)
	}
	to, err := StaticFromSeed(toSeed)
	if err != nil {
		t.Fatal(err)
	}
	ephemeral := mustHex(t, "ephemeral", doc.Handshake.EphemeralHex)
	msg1, msg2, err := handshake(from, to, bytes.NewReader(ephemeral))
	if err != nil {
		t.Fatal(err)
	}
	wantMsg1 := mustHex(t, "msg1", doc.Handshake.Message1Hex)
	wantMsg2 := mustHex(t, "msg2", doc.Handshake.Message2Hex)
	if !bytes.Equal(msg1, wantMsg1) {
		t.Fatalf("handshake msg1 = %x, want %x", msg1, wantMsg1)
	}
	if !bytes.Equal(msg2, wantMsg2) {
		t.Fatalf("handshake msg2 = %x, want %x", msg2, wantMsg2)
	}
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc vectorFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func mustHex(t *testing.T, name, encoded string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return raw
}
