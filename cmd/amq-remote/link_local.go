package main

import (
	"bufio"
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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// A local confirmation needs the person at this machine. The command needs a
// terminal, and the page it opens needs the local passkey (Touch ID). The
// ENDPOINT serves every local page, on one loopback port it holds for its
// whole life and records once per root, so the local passkey is bound to
// that origin (ruling cc): a look-alike page an agent serves on another port
// cannot produce a confirmation the endpoint accepts. No IPC operation takes
// an assertion; only the endpoint's own page does.

const (
	pagesFile        = "pages.json"
	registerDeadline = 5 * time.Minute
)

// stdinIsTerminal is the TTY check; tests replace it.
var stdinIsTerminal = func(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// openBrowser opens url for the person; tests replace it.
var openBrowser = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Run()
}

// pageResult is what a local page posts back: the WebAuthn response, base64url.
type pageResult struct {
	ClientDataJSON    string `json:"client_data_json"`
	AuthenticatorData string `json:"authenticator_data,omitempty"`
	Signature         string `json:"signature,omitempty"`
	AttestationObject string `json:"attestation_object,omitempty"`
}

// pageStatus is what the CLI reads about a page it asked for.
type pageStatus struct {
	URL      string    `json:"url,omitempty"`
	Token    string    `json:"token,omitempty"`
	Deadline time.Time `json:"deadline,omitzero"`
	Done     bool      `json:"done"`
	OK       bool      `json:"ok"`
	Message  string    `json:"message,omitempty"`
}

// localPage is one one-shot page the endpoint serves.
type localPage struct {
	html     string
	nonce    string
	deadline time.Time
	finish   func(pageResult) (string, error) // runs once, on the first good POST
	once     sync.Once
	done     chan struct{}
	ok       bool
	message  string
}

// pageServer is the endpoint's loopback page server.
type pageServer struct {
	stateDir string
	mu       sync.Mutex
	origin   string // http://localhost:<port>, set once listening
	port     int
	pages    map[string]*localPage
}

// ensure starts listening on the root's recorded port, recording a fresh one
// the first time. A recorded port that another process holds is an error:
// the endpoint never serves its pages from another origin.
func (ps *pageServer) ensure(ctx context.Context) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.origin != "" {
		return nil
	}
	path := filepath.Join(linkio.Dir(ps.stateDir), pagesFile)
	var rec struct {
		Port int `json:"port"`
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &rec)
	}
	l, err := net.Listen("tcp", "localhost:"+strconv.Itoa(rec.Port))
	if err != nil {
		return fmt.Errorf("the local page port %d is taken by another program; local confirmation is unavailable until it is free", rec.Port)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if rec.Port == 0 {
		data, _ := json.Marshal(map[string]int{"port": port})
		if err := os.MkdirAll(linkio.Dir(ps.stateDir), 0o700); err != nil {
			_ = l.Close()
			return err
		}
		if _, err := fsq.WriteFileAtomic(linkio.Dir(ps.stateDir), pagesFile, data, 0o600); err != nil {
			_ = l.Close()
			return err
		}
	}
	ps.port, ps.origin = port, fmt.Sprintf("http://localhost:%d", port)
	ps.pages = map[string]*localPage{}
	srv := &http.Server{Handler: http.HandlerFunc(ps.serve), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	return nil
}

// add registers a page and returns its status, with the URL to open.
func (ps *pageServer) add(p *localPage) pageStatus {
	token := randomToken()
	p.done = make(chan struct{})
	ps.mu.Lock()
	for t, old := range ps.pages {
		if time.Now().After(old.deadline) {
			delete(ps.pages, t)
		}
	}
	ps.pages[token] = p
	origin := ps.origin
	ps.mu.Unlock()
	return pageStatus{URL: origin + linkio.ConfirmPathPrefix + token + "/", Token: token, Deadline: p.deadline}
}

// wait reports a page's outcome, waiting up to d for it.
func (ps *pageServer) wait(token string, d time.Duration) (pageStatus, error) {
	ps.mu.Lock()
	p := ps.pages[token]
	ps.mu.Unlock()
	if p == nil {
		return pageStatus{}, protocol.Refuse(protocol.CodeNotFound, "no such page")
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.done:
		ps.mu.Lock()
		delete(ps.pages, token)
		ps.mu.Unlock()
		return pageStatus{Done: true, OK: p.ok, Message: p.message}, nil
	case <-t.C:
		if time.Now().After(p.deadline) {
			return pageStatus{Done: true, Message: "the page expired"}, nil
		}
		return pageStatus{}, nil
	}
}

// serve answers one page request. The 128-bit path token already defeats DNS
// rebinding and cross-site posts (both need the URL); the Host and Origin
// checks below are the cheap second wall.
func (ps *pageServer) serve(w http.ResponseWriter, r *http.Request) {
	ps.mu.Lock()
	port, origin := ps.port, ps.origin
	ps.mu.Unlock()
	switch r.Host {
	case "localhost:" + strconv.Itoa(port), "127.0.0.1:" + strconv.Itoa(port), "[::1]:" + strconv.Itoa(port):
	default:
		http.Error(w, "wrong host", http.StatusForbidden)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, linkio.ConfirmPathPrefix)
	token, action, _ := strings.Cut(rest, "/")
	ps.mu.Lock()
	p := ps.pages[token]
	ps.mu.Unlock()
	if !ok || p == nil || time.Now().After(p.deadline) {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'none'; script-src 'nonce-%s'; style-src 'nonce-%s'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'", p.nonce, p.nonce))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, p.html)
	case r.Method == http.MethodPost && action == "done":
		if r.Header.Get("Origin") != origin {
			http.Error(w, "wrong origin", http.StatusForbidden)
			return
		}
		var res pageResult
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&res); err != nil {
			http.Error(w, "unreadable answer", http.StatusBadRequest)
			return
		}
		ran := false
		p.once.Do(func() {
			ran = true
			msg, err := p.finish(res)
			p.ok, p.message = err == nil, msg
			if err != nil {
				p.message = err.Error()
			}
			close(p.done)
		})
		if !ran {
			http.Error(w, "this page was already answered", http.StatusConflict)
			return
		}
		if !p.ok {
			http.Error(w, p.message, http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, p.message)
	default:
		http.NotFound(w, r)
	}
}

// localPageRequest serves the link.v1 page ops for a link: open a register
// or confirm page, or wait for one's outcome.
func (ls *linkSet) localPageRequest(run *linkRun, name string, req ipc.LinkRequest) (json.RawMessage, error) {
	if req.Op == "page_result" {
		st, err := ls.pages.wait(req.Token, time.Duration(min(max(req.WaitMS, 0), 30000))*time.Millisecond)
		if err != nil {
			return nil, err
		}
		return json.Marshal(st)
	}
	if err := ls.pages.ensure(ls.ctx); err != nil {
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "%v", err)
	}
	ls.pages.mu.Lock()
	origin := ls.pages.origin
	ls.pages.mu.Unlock()
	var p *localPage
	switch req.Page {
	case "register":
		if linkio.HasLocalKey(ls.stateDir, name) {
			return nil, protocol.Refuse(protocol.CodeInvalid, "link %s already has a local passkey", name)
		}
		challenge := make([]byte, 32)
		_, _ = rand.Read(challenge)
		p = registerPage(origin, name, challenge, func(res pageResult) (string, error) {
			cd, err1 := base64.RawURLEncoding.DecodeString(res.ClientDataJSON)
			att, err2 := base64.RawURLEncoding.DecodeString(res.AttestationObject)
			if err := errors.Join(err1, err2); err != nil {
				return "", errors.New("the answer is not base64url")
			}
			key, err := linkio.VerifyLocalRegistration(challenge, origin, cd, att)
			if err != nil {
				return "", err
			}
			if err := linkio.WriteLocalKey(ls.stateDir, name, key); err != nil {
				return "", err
			}
			return "Created the local passkey for link " + name + ".", nil
		})
	case "confirm":
		if run == nil {
			return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "link %s is not running", name)
		}
		key, err := linkio.LoadLocalKey(ls.stateDir, name)
		if err != nil || key.Origin != origin {
			return nil, protocol.Refuse(protocol.CodeUnsupported, "link %s has no local passkey for this endpoint's page; run amq-remote link local-key %s", name, name)
		}
		task, err := run.carrier.HeldTask(req.ConfirmID)
		if err != nil {
			var r *linkio.Refusal
			if errors.As(err, &r) {
				return nil, protocol.Refuse(protocol.Code(r.Code), "%s", r.Message)
			}
			return nil, err
		}
		p = confirmPage(task, key, func(res pageResult) (string, error) {
			out, err := run.carrier.ConfirmLocal(task.ID, task.Digest, res.AuthenticatorData, res.ClientDataJSON, res.Signature)
			if err != nil {
				return "", err
			}
			return handedMessage(task.Session, out)
		})
	default:
		return nil, protocol.Refuse(protocol.CodeInvalid, "unknown page %q", req.Page)
	}
	return json.Marshal(ls.pages.add(p))
}

// handedMessage says what core did with a confirmed task: a refusal at the
// handoff is an Outcome code, and it is reported as one, never as handed.
func handedMessage(session string, out any) (string, error) {
	reply, ok := out.(protocol.Reply)
	if !ok {
		return "", fmt.Errorf("not handed to %s: the endpoint gave no answer", session)
	}
	if reply.Outcome.Code != "" {
		return "", fmt.Errorf("not handed to %s: %s %s", session, reply.Outcome.Code, reply.Outcome.Message)
	}
	return "Confirmed. The task was handed to " + session + ".", nil
}

func randomToken() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// visibleText escapes text for HTML and makes every character that could
// hide or reorder text visible, the same set the linked server's card marks:
// controls other than newline and tab, format characters (bidirectional
// controls, zero-width characters, tags), private use, the line and
// paragraph separators, and the variation selectors.
func visibleText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Zl, unicode.Zp) ||
			(r >= 0xFE00 && r <= 0xFE0F) || (r >= 0xE0100 && r <= 0xE01EF):
			fmt.Fprintf(&b, `<mark>U+%04X</mark>`, r)
		default:
			b.WriteString(html.EscapeString(string(r)))
		}
	}
	return b.String()
}

func pageShell(nonce, body, script string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>amq-remote</title><style nonce="` + nonce + `">
body{font:15px system-ui,sans-serif;max-width:46rem;margin:2rem auto;padding:0 1rem}
pre{white-space:pre-wrap;word-break:break-word;background:#f4f4f4;padding:1rem;border:1px solid #ccc}
mark{background:#fc6;font-size:.8em}button{font-size:1rem;padding:.5rem 1rem}#s{margin-top:1rem}</style></head><body>` +
		body + `<div id="s"></div><script nonce="` + nonce + `">
const d=s=>Uint8Array.from(atob(s.replace(/-/g,'+').replace(/_/g,'/')),c=>c.charCodeAt(0));
const e=b=>btoa(String.fromCharCode(...new Uint8Array(b))).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
const st=t=>document.getElementById('s').textContent=t;
async function send(res){const r=await fetch('done',{method:'POST',body:JSON.stringify(res)});st(await r.text());}
` + script + `</script></body></html>`
}

func registerPage(origin, name string, challenge []byte, finish func(pageResult) (string, error)) *localPage {
	nonce := randomToken()
	user := base64.RawURLEncoding.EncodeToString([]byte(name))
	script := fmt.Sprintf(`document.getElementById('b').onclick=async()=>{try{
const c=await navigator.credentials.create({publicKey:{challenge:d(%q),rp:{id:'localhost',name:'amq-remote'},user:{id:d(%q),name:%q,displayName:%q},
pubKeyCredParams:[{type:'public-key',alg:-7},{type:'public-key',alg:-8}],authenticatorSelection:{userVerification:'required',residentKey:'discouraged'},attestation:'none',timeout:120000}});
await send({client_data_json:e(c.response.clientDataJSON),attestation_object:e(c.response.attestationObject)});
}catch(x){st('Not created: '+x);}};`, base64.RawURLEncoding.EncodeToString(challenge), user, "amq-remote "+name, "amq-remote "+name)
	body := `<h1>Create the local confirmation passkey</h1><p>Tasks shared with "consent: local" wait until you confirm them on this page's address with Touch ID. Served by amq-remote at ` + html.EscapeString(origin) + `.</p><button id="b">Create with Touch ID</button>`
	return &localPage{html: pageShell(nonce, body, script), nonce: nonce, deadline: time.Now().Add(registerDeadline), finish: finish}
}

func confirmPage(task linkio.HeldTask, key linkio.ConsentKey, finish func(pageResult) (string, error)) *localPage {
	nonce := randomToken()
	challenge := base64.RawURLEncoding.EncodeToString(linkio.ConfirmChallenge(task.Digest))
	script := fmt.Sprintf(`document.getElementById('b').onclick=async()=>{try{
const c=await navigator.credentials.get({publicKey:{challenge:d(%q),rpId:'localhost',userVerification:'required',timeout:120000,allowCredentials:[{type:'public-key',id:d(%q)}]}});
await send({authenticator_data:e(c.response.authenticatorData),client_data_json:e(c.response.clientDataJSON),signature:e(c.response.signature)});
}catch(x){st('Not confirmed: '+x);}};`, challenge, key.CredentialID)
	body := fmt.Sprintf(`<h1>Run this task on %s?</h1><p>Task %s. Only Touch ID on this machine confirms it. It expires at %s.</p><pre>%s</pre><button id="b">Confirm with Touch ID</button>`,
		html.EscapeString(task.Session), html.EscapeString(task.ID), task.NotAfter.Local().Format(time.Kitchen), visibleText(task.Text))
	return &localPage{html: pageShell(nonce, body, script), nonce: nonce, deadline: task.NotAfter, finish: finish}
}

// followPage asks the endpoint for a page, opens it, and waits for its
// outcome until the page's deadline.
func followPage(stateDir string, req ipc.LinkRequest, stdout io.Writer) (pageStatus, error) {
	raw, err := linkIPC(stateDir, req)
	if err != nil {
		return pageStatus{}, err
	}
	var st pageStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return pageStatus{}, err
	}
	say(stdout, "Open %s", st.URL)
	if err := openBrowser(st.URL); err != nil {
		say(stdout, "(could not open a browser: open the address above yourself)")
	}
	for time.Now().Before(st.Deadline.Add(time.Second)) {
		raw, err := linkIPC(stateDir, ipc.LinkRequest{Name: req.Name, Op: "page_result", Token: st.Token, WaitMS: 30000})
		if err != nil {
			return pageStatus{}, err
		}
		var res pageStatus
		if err := json.Unmarshal(raw, &res); err != nil {
			return pageStatus{}, err
		}
		if res.Done {
			return res, nil
		}
	}
	return pageStatus{Done: true, Message: "the page expired"}, nil
}

// linkConfirm confirms one task that waits on this machine, on the page the
// endpoint serves.
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
	st, err := followPage(stateDir, ipc.LinkRequest{Name: *linkName, Op: "page", Page: "confirm", ConfirmID: pos[0]}, stdout)
	if err != nil {
		return nil, 0, err
	}
	if !st.OK {
		return nil, protocol.ExitActionRequired, errors.New(st.Message)
	}
	say(stdout, "%s", st.Message)
	return nil, 0, nil
}

// linkLocalKey creates a link's local confirmation passkey on the endpoint's
// page. It needs a terminal and a running endpoint, and never replaces a key.
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

func registerLocalKey(stateDir, name string, stdin io.Reader, stdout io.Writer) error {
	if !stdinIsTerminal(stdin) {
		return protocol.Refuse(protocol.CodeUnsupported, "a local passkey is created from a terminal: run amq-remote link local-key %s yourself", name)
	}
	st, err := followPage(stateDir, ipc.LinkRequest{Name: name, Op: "page", Page: "register"}, stdout)
	if err != nil {
		return err
	}
	if !st.OK {
		return errors.New(st.Message)
	}
	say(stdout, "%s", st.Message)
	return nil
}

// offerLocalKey asks once, after linking, whether to create the local
// passkey now. It is only needed for consent: local, and needs a browser.
func offerLocalKey(stateDir, name string, in *bufio.Reader, stdin io.Reader, stdout io.Writer) {
	say(stdout, "Create the local confirmation passkey now? Only needed for shares with consent: local. [y/N]")
	answer, _ := in.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		say(stdout, "Create it later with amq-remote link local-key %s.", name)
		return
	}
	if err := registerLocalKey(stateDir, name, stdin, stdout); err != nil {
		say(stdout, "No local passkey yet (%v). Create it later with amq-remote link local-key %s.", err, name)
	}
}
