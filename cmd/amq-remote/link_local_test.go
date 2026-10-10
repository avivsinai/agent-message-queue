package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// link confirm needs the person at the machine: without a terminal it
// refuses before it reads anything.
func TestLinkConfirmNeedsATerminal(t *testing.T) {
	_, _, err := linkConfirm([]string{"--root", t.TempDir(), "7f3a2c1e"}, strings.NewReader(""), io.Discard, &jsonProbe{})
	if protocol.RefusalCode(err) != protocol.CodeUnsupported || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %v, want a refusal that names the terminal", err)
	}
}

// The endpoint serves the registration page on its own loopback port: a
// browser with a passkey answers it, the link keeps that key bound to the
// page's origin, and a post that does not come from that origin is refused.
func TestLocalKeyRegistrationOnTheEndpointPage(t *testing.T) {
	root, err := os.MkdirTemp("", "arp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, stateDirName)
	if _, err := linkio.MintDeviceKey(stateDir, "example"); err != nil {
		t.Fatal(err)
	}
	mf := manifest.File{SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
		Links: []manifest.Link{{Name: "example", URL: "wss://link.example.test/link"}}}
	if err := manifest.Write(manifest.DefaultPath(stateDir), mf); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ls := newLinkSet(root, stateDir, manifest.DefaultPath(stateDir), "test", io.Discard)
	ls.ctx = ctx
	srv, err := ipc.Listen(stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetLinkHandler(ls.request)
	go func() { _ = srv.Serve(ctx) }()

	seed := sha256.Sum256([]byte("amq.remote.link/1 test local key"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	var origin string
	defer func(f func(io.Reader) bool, o func(string) error) { stdinIsTerminal, openBrowser = f, o }(stdinIsTerminal, openBrowser)
	stdinIsTerminal = func(io.Reader) bool { return true }
	openBrowser = func(url string) error { // the browser: load the page, create a passkey, post it
		origin = url[:strings.Index(url, linkio.ConfirmPathPrefix)]
		go func() {
			resp, err := http.Get(url)
			if err != nil {
				t.Error(err)
				return
			}
			page, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			m := regexp.MustCompile(`challenge:d\("([^"]+)"\)`).FindSubmatch(page)
			if m == nil {
				t.Error("no challenge on the page")
				return
			}
			cd, att := attestLocal(priv, string(m[1]), origin)
			body, _ := json.Marshal(pageResult{ClientDataJSON: b64(cd), AttestationObject: b64(att)})
			if code := post(url+"done", "http://localhost:1", body); code != http.StatusForbidden {
				t.Errorf("a post from another origin = %d, want 403", code)
			}
			if code := post(url+"done", origin, body); code != http.StatusOK {
				t.Errorf("the page's own post = %d, want 200", code)
			}
		}()
		return nil
	}
	if err := registerLocalKey(stateDir, "example", strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	key, err := linkio.LoadLocalKey(stateDir, "example")
	if err != nil || key.Alg != -8 || key.RPID != linkio.LocalRPID || key.Origin != origin {
		t.Fatalf("local key %+v, %v; want bound to %s", key, err, origin)
	}
}

func post(url, origin string, body []byte) int {
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Every character that can hide or reorder text is shown as U+XXXX, the same
// set the linked server's card marks; the rest is HTML-escaped.
func TestVisibleTextMarksHiddenCharacters(t *testing.T) {
	got := visibleText("a<b>\u202e\u200b\U000E0041\uFE0F\uE000z")
	want := "a&lt;b&gt;<mark>U+202E</mark><mark>U+200B</mark><mark>U+E0041</mark><mark>U+FE0F</mark><mark>U+E000</mark>z"
	if got != want {
		t.Fatalf("visibleText = %q, want %q", got, want)
	}
}

// A confirmed task that core refused at the handoff is reported as not
// handed, with the code, never as handed.
func TestHandedMessageReportsARefusedHandoff(t *testing.T) {
	_, err := handedMessage("pi · demo", protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Code: protocol.CodeSessionChanged}})
	if err == nil || !strings.Contains(err.Error(), "not handed to pi · demo: session_changed") {
		t.Fatalf("err = %v", err)
	}
	if msg, err := handedMessage("pi · demo", protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}); err != nil || !strings.Contains(msg, "handed to pi · demo") {
		t.Fatalf("msg = %q, %v", msg, err)
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// attestLocal is a software authenticator's navigator.credentials.create
// answer for the localhost relying party: an Ed25519 key, attestation none.
func attestLocal(priv ed25519.PrivateKey, challenge, origin string) (clientData, attestation []byte) {
	clientData = []byte(`{"type":"webauthn.create","challenge":"` + challenge + `","origin":"` + origin + `","crossOrigin":false}`)
	rp := sha256.Sum256([]byte(linkio.LocalRPID))
	auth := append(rp[:], 0x01|0x04|0x40) // UP, UV, attested credential data
	auth = binary.BigEndian.AppendUint32(auth, 0)
	auth = append(auth, make([]byte, 16)...)
	auth = binary.BigEndian.AppendUint16(auth, 6)
	auth = append(auth, "cred-1"...)
	auth = append(auth, 0xa4, 0x01, 0x01, 0x03, 0x27, 0x20, 0x06, 0x21, 0x58, 0x20)
	auth = append(auth, priv.Public().(ed25519.PublicKey)...)
	att := []byte{0xa3, 0x63, 'f', 'm', 't', 0x64, 'n', 'o', 'n', 'e', 0x67, 'a', 't', 't', 'S', 't', 'm', 't', 0xa0,
		0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x59}
	att = binary.BigEndian.AppendUint16(att, uint16(len(auth)))
	return clientData, append(att, auth...)
}
