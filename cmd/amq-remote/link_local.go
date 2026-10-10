package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// A local confirmation needs the person at this machine: the command needs a
// terminal, and the page it opens needs the local passkey (Touch ID). The
// page is served by amq-remote on localhost under linkio.ConfirmPathPrefix,
// the one prefix the linked server's browser extension may never drive.

// stdinIsTerminal is the TTY check; tests replace it.
var stdinIsTerminal = func(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// openBrowser opens url for the person; tests replace it.
var openBrowser = func(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, url).Start()
}

// pageResult is what a local page posts back: the WebAuthn response, base64url.
type pageResult struct {
	ClientDataJSON    string `json:"client_data_json"`
	AuthenticatorData string `json:"authenticator_data,omitempty"`
	Signature         string `json:"signature,omitempty"`
	AttestationObject string `json:"attestation_object,omitempty"`
}

// localPage serves one page under a fresh random path on localhost and
// returns what the page posts, or the reason it gave up. finish answers the
// page with the outcome before the server closes.
func localPage(ctx context.Context, body string, stdout io.Writer, finish func(pageResult) (string, error)) error {
	token := randomToken()
	base := linkio.ConfirmPathPrefix + token + "/"
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("POST "+base+"done", func(w http.ResponseWriter, r *http.Request) {
		var res pageResult
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&res); err != nil {
			http.Error(w, "unreadable answer", http.StatusBadRequest)
			return
		}
		msg, err := finish(res)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			done <- err
			return
		}
		_, _ = io.WriteString(w, msg)
		done <- nil
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	defer func() { // let the answer to the page finish before closing
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	port := l.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://localhost:%d%s", port, base)
	say(stdout, "Opening %s", url)
	openBrowser(url)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errors.New("no confirmation in time")
	}
}

func randomToken() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// visibleText escapes text for HTML and makes every character that could
// hide or reorder text visible: controls other than newline and tab, and
// format characters (bidirectional controls, zero-width characters).
func visibleText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ':
			fmt.Fprintf(&b, `<mark class="hidden">U+%04X</mark>`, r)
		default:
			b.WriteString(html.EscapeString(string(r)))
		}
	}
	return b.String()
}

const pageHead = `<!doctype html><html><head><meta charset="utf-8"><title>amq-remote</title>
<style>body{font:15px system-ui,sans-serif;max-width:46rem;margin:2rem auto;padding:0 1rem}
pre{white-space:pre-wrap;word-break:break-word;background:#f4f4f4;padding:1rem;border:1px solid #ccc}
mark.hidden{background:#fc6;font-size:.8em}button{font-size:1rem;padding:.5rem 1rem}#s{margin-top:1rem}</style></head><body>`

const pageScript = `<script>
const d=s=>Uint8Array.from(atob(s.replace(/-/g,'+').replace(/_/g,'/')),c=>c.charCodeAt(0));
const e=b=>btoa(String.fromCharCode(...new Uint8Array(b))).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
const st=t=>document.getElementById('s').textContent=t;
async function send(res){const r=await fetch('done',{method:'POST',body:JSON.stringify(res)});st(await r.text());}
</script>`

// linkConfirm confirms one task that waits on this machine: it shows the
// exact signed text on a local page, and the local passkey confirms it.
func linkConfirm(args []string, stdin io.Reader, stdout io.Writer, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link confirm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	linkName := fs.String("link", "", "the link the task came from (default: the only one)")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "usage: amq-remote link confirm ID")
	}
	if !stdinIsTerminal(stdin) {
		return nil, protocol.ExitActionRequired, protocol.Refuse(protocol.CodeUnsupported, "link confirm needs a terminal: run it yourself")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	raw, err := linkIPC(stateDir, ipc.LinkRequest{Name: *linkName, Op: "held"})
	if err != nil {
		return nil, 0, err
	}
	var held []linkio.HeldTask
	if err := json.Unmarshal(raw, &held); err != nil {
		return nil, protocol.ExitError, err
	}
	var task *linkio.HeldTask
	for i := range held {
		if held[i].ID == pos[0] {
			task = &held[i]
		}
	}
	if task == nil {
		return nil, protocol.ExitNotFound, protocol.Refuse(protocol.CodeNotFound, "no task %s waits for confirmation (it may have expired)", pos[0])
	}
	name, err := oneLinkName(stateDir, *linkName)
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	key, err := linkio.LoadLocalKey(stateDir, name)
	if err != nil {
		return nil, protocol.ExitActionRequired, fmt.Errorf("link %s has no local confirmation passkey; run amq-remote link local-key %s", name, name)
	}
	challenge := base64.RawURLEncoding.EncodeToString(linkio.ConfirmChallenge(task.Digest))
	page := pageHead + fmt.Sprintf(`<h1>Run this task on %s?</h1><p>Only Touch ID on this machine confirms it. Expires %s.</p><pre>%s</pre>
<button id="b">Confirm with Touch ID</button><div id="s"></div>`, html.EscapeString(task.Session), task.NotAfter.Local().Format(time.Kitchen), visibleText(task.Text)) +
		pageScript + fmt.Sprintf(`<script>document.getElementById('b').onclick=async()=>{try{
const c=await navigator.credentials.get({publicKey:{challenge:d(%q),rpId:'localhost',userVerification:'required',timeout:120000,allowCredentials:[{type:'public-key',id:d(%q)}]}});
await send({authenticator_data:e(c.response.authenticatorData),client_data_json:e(c.response.clientDataJSON),signature:e(c.response.signature)});
}catch(x){st('Not confirmed: '+x);}};</script></body></html>`, challenge, key.CredentialID)
	ctx, cancel := context.WithDeadline(context.Background(), task.NotAfter)
	defer cancel()
	var outcome json.RawMessage
	err = localPage(ctx, page, stdout, func(res pageResult) (string, error) {
		out, err := linkIPC(stateDir, ipc.LinkRequest{Name: name, Op: "confirm", Confirm: &ipc.LocalConfirm{
			ID: task.ID, Digest: task.Digest, AuthenticatorData: res.AuthenticatorData,
			ClientDataJSON: res.ClientDataJSON, Signature: res.Signature,
		}})
		if err != nil {
			return "", err
		}
		outcome = out
		return "Confirmed. The task was handed to " + task.Session + ".", nil
	})
	if err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	if c.json {
		return outcome, 0, nil
	}
	say(stdout, "Confirmed: task %s was handed to %s.", task.ID, task.Session)
	return nil, 0, nil
}

// linkLocalKey registers the local confirmation passkey of a link (relying
// party localhost) on a page amq-remote serves. It needs a terminal, and it
// never replaces a key: remove the link and add it again for a new one.
func linkLocalKey(args []string, stdin io.Reader, stdout io.Writer, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link local-key", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "usage: amq-remote link local-key NAME")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	if err := registerLocalKey(stateDir, pos[0], stdin, stdout); err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	return nil, 0, nil
}

// registerLocalKey runs the registration page for a link's local passkey.
func registerLocalKey(stateDir, name string, stdin io.Reader, stdout io.Writer) error {
	if !stdinIsTerminal(stdin) {
		return protocol.Refuse(protocol.CodeUnsupported, "a local passkey is registered from a terminal: run amq-remote link local-key %s yourself", name)
	}
	if _, err := linkio.LoadDeviceKey(stateDir, name); err != nil {
		return fmt.Errorf("no link named %q in this root", name)
	}
	if linkio.HasLocalKey(stateDir, name) {
		return fmt.Errorf("link %s already has a local passkey", name)
	}
	challenge := make([]byte, 32)
	_, _ = rand.Read(challenge)
	page := pageHead + `<h1>Create the local confirmation passkey</h1><p>Tasks you mark "consent: local" wait until you confirm them here with Touch ID.</p>
<button id="b">Create with Touch ID</button><div id="s"></div>` + pageScript + fmt.Sprintf(`<script>document.getElementById('b').onclick=async()=>{try{
const c=await navigator.credentials.create({publicKey:{challenge:d(%q),rp:{id:'localhost',name:'amq-remote'},user:{id:d(%q),name:%q,displayName:%q},
pubKeyCredParams:[{type:'public-key',alg:-7},{type:'public-key',alg:-8}],authenticatorSelection:{userVerification:'required',residentKey:'discouraged'},attestation:'none',timeout:120000}});
await send({client_data_json:e(c.response.clientDataJSON),attestation_object:e(c.response.attestationObject)});
}catch(x){st('Not created: '+x);}};</script></body></html>`,
		base64.RawURLEncoding.EncodeToString(challenge), base64.RawURLEncoding.EncodeToString([]byte(name)), "amq-remote "+name, "amq-remote "+name)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return localPage(ctx, page, stdout, func(res pageResult) (string, error) {
		cd, err1 := base64.RawURLEncoding.DecodeString(res.ClientDataJSON)
		att, err2 := base64.RawURLEncoding.DecodeString(res.AttestationObject)
		if err := errors.Join(err1, err2); err != nil {
			return "", errors.New("the answer is not base64url")
		}
		key, err := linkio.VerifyLocalRegistration(challenge, cd, att)
		if err != nil {
			return "", err
		}
		if err := linkio.WriteLocalKey(stateDir, name, key); err != nil {
			return "", err
		}
		say(stdout, "Local passkey created for link %s.", name)
		return "Created. You can close this page.", nil
	})
}

// oneLinkName resolves --link, or the only link of the root.
func oneLinkName(stateDir, name string) (string, error) {
	if name != "" {
		return name, nil
	}
	mf, err := manifestLinks(stateDir)
	if err != nil {
		return "", err
	}
	if len(mf) != 1 {
		return "", fmt.Errorf("this root has %d links; name one with --link", len(mf))
	}
	return mf[0], nil
}

// manifestLinks lists the link names in the root's manifest.
func manifestLinks(stateDir string) ([]string, error) {
	mf, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(mf.Links))
	for _, l := range mf.Links {
		out = append(out, l.Name)
	}
	return out, nil
}
