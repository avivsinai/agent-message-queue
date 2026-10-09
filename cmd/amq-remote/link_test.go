package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// link remove retires the sink and deletes the device key; afterwards the
// router settles that sink's records without any network, while a sink that
// is merely not running stays owed.
func TestLinkRemoveRetiresSink(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, stateDirName)
	key, err := linkio.MintDeviceKey(stateDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	if err := linkio.WritePin(stateDir, "example", linkio.Pin{ServerID: "srv_example"}); err != nil {
		t.Fatal(err)
	}
	mf := manifest.File{SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
		Links: []manifest.Link{{Name: "example", URL: "wss://link.example.test/link"}}}
	if err := manifest.Write(manifest.DefaultPath(stateDir), mf); err != nil {
		t.Fatal(err)
	}

	if _, code, err := linkRemove([]string{"--root", root, "example"}, io.Discard, &jsonProbe{}); err != nil || code != 0 {
		t.Fatalf("link remove = %d, %v", code, err)
	}
	if _, err := os.Stat(filepath.Join(linkio.LinkDir(stateDir, "example"), "device.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("device key still present: %v", err)
	}
	if got, err := manifest.Load(manifest.DefaultPath(stateDir)); err != nil || len(got.Links) != 0 {
		t.Fatalf("manifest still lists the link: %+v, %v", got.Links, err)
	}

	ls := newLinkSet(root, stateDir, manifest.DefaultPath(stateDir), "test", io.Discard)
	snap := protocol.Snapshot{RequestRef: "amqr1_example", Revision: 3}
	if err := ls.publish(snap, map[string]string{"carrier": "link", "sink": key.Host()}); err != nil {
		t.Fatalf("publish for the retired sink = %v, want nil (settled)", err)
	}
	if err := ls.publish(snap, map[string]string{"carrier": "link", "sink": "link-0000000000000000"}); !errors.Is(err, errCarrierUnavailable) {
		t.Fatalf("publish for a sink that is not running = %v, want unavailable", err)
	}
}

// link add redeems the code, pins the consent key whose fingerprint the user
// typed, and declares the link in the manifest.
func TestLinkAddPinsTypedFingerprint(t *testing.T) {
	ck := linkio.ConsentKey{CredentialID: "C32Z2-E6ARE-BkrOVOybpw", SPKI: "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE", Alg: -7,
		RPID: "sign.example.test", Origin: "https://sign.example.test"}
	fp, err := linkio.Fingerprint(ck)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/link/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(linkInfo{ServerID: "srv_example", LinkURL: "wss://link.example.test/api/v1/link"})
	})
	mux.HandleFunc("POST /api/v1/link/redeem", func(w http.ResponseWriter, r *http.Request) {
		var req redeemRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		spki, _ := base64.RawURLEncoding.DecodeString(req.DeviceKey)
		pub, err := x509.ParsePKIXPublicKey(spki)
		sig, _ := base64.RawURLEncoding.DecodeString(req.Signature)
		if err != nil || req.Code != "K7Q4-M2XD" || !ed25519.Verify(pub.(ed25519.PublicKey), linkio.RedeemMessage("srv_example", req.Code, req.TS), sig) {
			http.Error(w, "bad redeem", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(redeemReply{DeviceID: linkio.HostOf(spki), ServerID: "srv_example", User: "Example User", ConsentKeys: []linkio.ConsentKey{ck}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	stateDir := filepath.Join(root, stateDirName)
	typed := strings.NewReader(strings.ToLower(fp) + "\n")
	if _, code, err := linkAdd([]string{"--root", root, "example", srv.URL, "--code", "K7Q4-M2XD"}, typed, io.Discard, &jsonProbe{}); err != nil || code != 0 {
		t.Fatalf("link add = %d, %v", code, err)
	}
	pin, err := linkio.LoadPin(stateDir, "example")
	if err != nil || pin.ServerID != "srv_example" || pin.URL != "wss://link.example.test/api/v1/link" {
		t.Fatalf("pin %+v, %v", pin, err)
	}
	keys, err := linkio.LoadConsentKeys(stateDir, "example")
	if err != nil || len(keys) != 1 || keys[0] != ck {
		t.Fatalf("consent keys %+v, %v", keys, err)
	}
	mf, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil || len(mf.Links) != 1 || mf.Links[0].Name != "example" {
		t.Fatalf("manifest links %+v, %v", mf.Links, err)
	}
}
