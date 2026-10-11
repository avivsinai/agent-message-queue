package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
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

// Regression (#1026 review B1): after the server revoked the device (close
// 4010), the endpoint retired the sink and deleted the link dir, so `link
// remove` said "no link" and the manifest kept the dead link forever. Now
// `link status` shows the link as retired, and `link remove` drops it.
func TestLinkRemoveAfterServerRevoke(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, stateDirName)
	key, err := linkio.MintDeviceKey(stateDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	mf := manifest.File{SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
		Links: []manifest.Link{{Name: "example", URL: "wss://link.example.test/link"}}}
	if err := manifest.Write(manifest.DefaultPath(stateDir), mf); err != nil {
		t.Fatal(err)
	}
	if _, err := linkio.Retire(stateDir, "example"); err != nil { // what close 4010 does
		t.Fatal(err)
	}
	out, _, err := linkStatus([]string{"--root", root}, &jsonProbe{})
	st, _ := out.(linkStatusOut)
	if err != nil || len(st.Links) != 1 || st.Links[0].Status.State != "retired" || st.Links[0].Sink != key.Host() {
		t.Fatalf("link status = %+v, %v; want the link shown retired with its sink", out, err)
	}
	if _, code, err := linkRemove([]string{"--root", root, "example"}, io.Discard, &jsonProbe{}); err != nil || code != 0 {
		t.Fatalf("link remove = %d, %v", code, err)
	}
	if got, err := manifest.Load(manifest.DefaultPath(stateDir)); err != nil || len(got.Links) != 0 {
		t.Fatalf("manifest links %+v, %v; want none", got.Links, err)
	}
}

// Sharing a session with a link puts its share, with consent and tools, on
// that link in the manifest; sharing it again replaces the share. (attach
// --self --link needs a live harness session to resolve, so this drives the
// manifest step it ends with.)
func TestShareWithLinkRecordsTheShare(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), stateDirName)
	mf := manifest.File{SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
		Links: []manifest.Link{{Name: "example", URL: "wss://link.example.test/link"}}}
	if err := manifest.Write(manifest.DefaultPath(stateDir), mf); err != nil {
		t.Fatal(err)
	}
	if err := shareWithLink(stateDir, "example", manifest.LinkShare{Binding: "pi-demo", Consent: "passkey", Tools: "read"}); err != nil {
		t.Fatal(err)
	}
	if err := shareWithLink(stateDir, "example", manifest.LinkShare{Binding: "pi-demo", Consent: "local"}); err != nil {
		t.Fatal(err)
	}
	got, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil || len(got.Links[0].Shares) != 1 || got.Links[0].Shares[0] != (manifest.LinkShare{Binding: "pi-demo", Consent: "local"}) {
		t.Fatalf("shares %+v, %v; want the one replaced share", got.Links, err)
	}
	if err := shareWithLink(stateDir, "other", manifest.LinkShare{Binding: "pi-demo", Consent: "passkey"}); err == nil {
		t.Fatal("sharing with an unknown link succeeded")
	}
}

// link keys add accepts one more consent passkey: the device redeems the
// code with its signature over the pinned server id, and the key is kept only
// when the user types its fingerprint.
func TestLinkKeysAddAcceptsTheTypedKey(t *testing.T) {
	first := linkio.ConsentKey{CredentialID: "C32Z2-E6ARE-BkrOVOybpw", SPKI: "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE", Alg: -7,
		RPID: "sign.example.test", Origin: "https://sign.example.test"}
	second := first
	second.CredentialID, second.Alg = "D43a3-F7BSF-ClsPWPzcqx", -8
	next := second
	redeem := func(ck *linkio.ConsentKey) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req redeemRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			spki, _ := base64.RawURLEncoding.DecodeString(req.DeviceKey)
			pub, err := x509.ParsePKIXPublicKey(spki)
			sig, _ := base64.RawURLEncoding.DecodeString(req.Signature)
			if err != nil || !ed25519.Verify(pub.(ed25519.PublicKey), linkio.RedeemMessage("srv_example", req.Code, req.TS), sig) {
				http.Error(w, "bad redeem", http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(redeemReply{DeviceID: linkio.HostOf(spki), ServerID: "srv_example", User: "Example User", ConsentKeys: []linkio.ConsentKey{*ck}})
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/link/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(linkInfo{ServerID: "srv_example", LinkURL: "wss://link.example.test/api/v1/link"})
	})
	mux.HandleFunc("POST /api/v1/link/redeem", redeem(&first))
	mux.HandleFunc("POST /api/v1/link/consent-keys/redeem", redeem(&next))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	fp1, _ := linkio.Fingerprint(first)
	if _, code, err := linkAdd([]string{"--root", root, "example", srv.URL, "--code", "K7Q4-M2XD"}, strings.NewReader(fp1+"\n"), io.Discard, &jsonProbe{}); err != nil || code != 0 {
		t.Fatalf("link add = %d, %v", code, err)
	}
	fp2, _ := linkio.Fingerprint(second)
	if _, code, err := linkKeysAdd([]string{"--root", root, "example", "--code", "P9R2-X4KD"}, strings.NewReader(fp2+"\n"), io.Discard, &jsonProbe{}); err != nil || code != 0 {
		t.Fatalf("link keys add = %d, %v", code, err)
	}
	keys, err := linkio.LoadConsentKeys(filepath.Join(root, stateDirName), "example")
	if err != nil || len(keys) != 2 || keys[1] != second {
		t.Fatalf("consent keys %+v, %v; want the first and the added one", keys, err)
	}
	// A key for another signing origin is refused even with its fingerprint.
	next.CredentialID, next.Origin, next.RPID = "E54b4-G8CTG-DmtQXQadry", "https://other.example.test", "other.example.test"
	fp3, _ := linkio.Fingerprint(next)
	if _, _, err := linkKeysAdd([]string{"--root", root, "example", "--code", "Q1S3-Y5LE"}, strings.NewReader(fp3+"\n"), io.Discard, &jsonProbe{}); err == nil {
		t.Fatal("a key for another origin was accepted")
	}
}

// Regression (compat pass, ruling rr): an endpoint older than links, still
// running after an upgrade, would register an attach and rewrite the
// manifest without the links block. link add and attach --link refuse while
// one runs, and pass once the running endpoint serves links.
func TestLinkRefusesAnOlderRunningEndpoint(t *testing.T) {
	root, err := os.MkdirTemp("", "aro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, stateDirName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := requireLinkEndpoint(stateDir); err != nil {
		t.Fatalf("no endpoint running = %v, want ok", err)
	}
	l, err := net.Listen("unix", ipc.SocketPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() { // an endpoint that predates the features question
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(c).ReadBytes('\n')
			_, _ = c.Write([]byte(`{"error":{"code":"invalid","message":"request carries neither command nor wait"}}` + "\n"))
			_ = c.Close()
		}
	}()
	err = requireLinkEndpoint(stateDir)
	if err == nil || !strings.Contains(err.Error(), "restart amq-remote up first") {
		t.Fatalf("older endpoint = %v, want the restart refusal", err)
	}
	_, _, err = linkAdd([]string{"--root", root, "example", "https://link.example.test", "--code", "K7Q4-M2XD"}, strings.NewReader(""), io.Discard, &jsonProbe{})
	if err == nil || !strings.Contains(err.Error(), "restart amq-remote up first") {
		t.Fatalf("link add with an older endpoint = %v, want the restart refusal", err)
	}
}

// Regression (merged-code e2e finding 2): when the shared session's native
// session changed (a pi /new or reload), the share silently dropped off the
// link and `link status` said nothing. It now names the state and the fix.
func TestLinkStatusNamesAShareWhoseSessionChanged(t *testing.T) {
	// A unit test of linkShareState, not of the link status row. The root is
	// short (os.MkdirTemp("")) because the endpoint's unix socket lives under
	// it and macOS caps socket paths at 104 bytes.
	root, err := os.MkdirTemp("", "arsc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	bindDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(binding.EnvPath, filepath.Join(bindDir, "remote", "binding.json"))
	stateDir := filepath.Join(root, stateDirName)
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ep := core.New(core.Config{Store: store})
	t.Cleanup(func() { _ = ep.Close() })
	ep.Register(fake.New("fake", "e_1")) // its native session is "fake"
	srv, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	for name, native := range map[string]string{"now": "fake", "old": "an-earlier-session"} {
		if err := binding.WriteNamed(binding.Binding{Root: root, Target: "fake", NativeSession: native, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	natives := &nativeProbe{stateDir: stateDir}
	if got := linkShareState(root, natives, "now"); got != "shared" {
		t.Fatalf("current session = %q, want shared", got)
	}
	if got := linkShareState(root, natives, "old"); !strings.HasPrefix(got, "session changed") {
		t.Fatalf("changed session = %q, want 'session changed: ...'", got)
	}
}
