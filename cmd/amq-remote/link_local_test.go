package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
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

// link local-key serves a registration page on localhost; a browser with a
// passkey answers it, and the link keeps that key for its confirmations.
func TestLocalKeyRegistrationPage(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), stateDirName)
	if _, err := linkio.MintDeviceKey(stateDir, "example"); err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte("amq.remote.link/1 test local key"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	defer func(f func(io.Reader) bool, o func(string)) { stdinIsTerminal, openBrowser = f, o }(stdinIsTerminal, openBrowser)
	stdinIsTerminal = func(io.Reader) bool { return true }
	openBrowser = func(url string) { // the browser: load the page, create a passkey, post it
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
			origin := strings.TrimSuffix(url[:strings.Index(url, linkio.ConfirmPathPrefix)], "/")
			cd, att := attestLocal(priv, string(m[1]), origin)
			body, _ := json.Marshal(pageResult{ClientDataJSON: b64(cd), AttestationObject: b64(att)})
			resp, err = http.Post(url+"done", "application/json", bytes.NewReader(body))
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Errorf("post = %v, %v", resp, err)
			}
		}()
	}
	if err := registerLocalKey(stateDir, "example", strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	key, err := linkio.LoadLocalKey(stateDir, "example")
	if err != nil || key.Alg != -8 || key.RPID != linkio.LocalRPID {
		t.Fatalf("local key %+v, %v", key, err)
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
