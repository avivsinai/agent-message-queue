package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// linkSet runs one carrier per linked server in the manifest and routes each
// link record's publication to the carrier of its sink (origin "sink", the
// creator host). A retired sink publishes nothing: its records settle their
// caller-delivery part without network.
type linkSet struct {
	root, stateDir, version string
	manifestFile            string
	stderr                  io.Writer

	mu       sync.Mutex
	ep       *core.Endpoint
	ctx      context.Context
	running  map[string]*linkRun // by link name
	byHost   map[string]*linkio.Carrier
	retired  map[string]bool
	mfStamp  time.Time
	statuses map[string]linkio.Status
}

type linkRun struct {
	carrier *linkio.Carrier
	cancel  context.CancelFunc
	link    manifest.Link
}

// newLinkSet loads the retired sinks, so records of a retired sink settle
// during startup reconciliation, before any link connects.
func newLinkSet(root, stateDir, manifestFile, version string, stderr io.Writer) *linkSet {
	retired, err := linkio.Retired(stateDir)
	if err != nil {
		say(stderr, "link: read retired sinks: %v", err)
		retired = map[string]bool{}
	}
	return &linkSet{
		root: root, stateDir: stateDir, manifestFile: manifestFile, version: version, stderr: stderr,
		running: map[string]*linkRun{}, byHost: map[string]*linkio.Carrier{}, retired: retired,
		statuses: map[string]linkio.Status{},
	}
}

// publish is the "link" entry of the publish router.
func (ls *linkSet) publish(s protocol.Snapshot, origin map[string]string) error {
	host := origin["sink"]
	ls.mu.Lock()
	retired, c := ls.retired[host], ls.byHost[host]
	ls.mu.Unlock()
	switch {
	case retired:
		return nil
	case c == nil:
		return fmt.Errorf("%w: link sink %q", errCarrierUnavailable, host)
	}
	return c.Publish(s, origin)
}

// start begins dialing every linked server once the endpoint can answer.
func (ls *linkSet) start(ctx context.Context, ep *core.Endpoint, mf manifest.File) {
	ls.mu.Lock()
	ls.ep, ls.ctx = ep, ctx
	ls.mu.Unlock()
	ls.sync(mf)
	if info, err := os.Stat(ls.manifestFile); err == nil {
		ls.mfStamp = info.ModTime()
	}
}

// tick runs on every sweep: it follows manifest changes (a link added,
// removed or a binding shared), tells each server when its bindings
// changed, and records each link's status for `link status`.
func (ls *linkSet) tick() {
	if info, err := os.Stat(ls.manifestFile); err == nil && !info.ModTime().Equal(ls.mfStamp) {
		ls.mfStamp = info.ModTime()
		if mf, err := manifest.Load(ls.manifestFile); err == nil {
			ls.sync(mf)
		} else {
			say(ls.stderr, "link: reload manifest: %v", err)
		}
	}
	ls.mu.Lock()
	runs := make([]*linkRun, 0, len(ls.running))
	for _, r := range ls.running {
		runs = append(runs, r)
	}
	ls.mu.Unlock()
	for _, r := range runs {
		r.carrier.Tick()
		st := r.carrier.Status()
		if prev, ok := ls.statuses[r.link.Name]; !ok || prev != st {
			ls.statuses[r.link.Name] = st
			writeLinkStatus(ls.stateDir, r.link.Name, st)
		}
	}
}

// sync starts a carrier for each manifest link that has a device key and is
// not retired, and stops carriers whose link is gone.
func (ls *linkSet) sync(mf manifest.File) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.ctx == nil {
		return
	}
	if retired, err := linkio.Retired(ls.stateDir); err == nil {
		ls.retired = retired
	}
	want := map[string]manifest.Link{}
	for _, l := range mf.Links {
		want[l.Name] = l
	}
	for name, r := range ls.running {
		if l, ok := want[name]; !ok || ls.retired[r.carrier.Host()] {
			r.cancel()
			delete(ls.byHost, r.carrier.Host())
			delete(ls.running, name)
		} else {
			r.link = l // shares changed: Bindings reads r.link
		}
	}
	for name, l := range want {
		if ls.running[name] != nil {
			continue
		}
		run, err := ls.newRun(l)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				say(ls.stderr, "link %s: %v", name, err)
			}
			continue
		}
		ctx, cancel := context.WithCancel(ls.ctx)
		run.cancel = cancel
		ls.running[name] = run
		ls.byHost[run.carrier.Host()] = run.carrier
		go func() {
			if err := run.carrier.Run(ctx); errors.Is(err, linkio.ErrRevoked) {
				say(ls.stderr, "link %s: the server revoked this device; the link is retired", name)
			}
		}()
	}
}

func (ls *linkSet) newRun(l manifest.Link) (*linkRun, error) {
	key, err := linkio.LoadDeviceKey(ls.stateDir, l.Name)
	if err != nil {
		return nil, err
	}
	if ls.retired[key.Host()] {
		return nil, fmt.Errorf("sink %s is retired", key.Host())
	}
	pin, err := linkio.LoadPin(ls.stateDir, l.Name)
	if err != nil {
		return nil, err
	}
	pin.URL = l.URL
	storeID, err := linkio.StoreID(ls.stateDir)
	if err != nil {
		return nil, err
	}
	run := &linkRun{link: l}
	name := l.Name
	c, err := linkio.New(linkio.Config{
		Name: name, Pin: pin, StoreID: storeID, Key: key, Version: version,
		Bindings: func() []linkio.Binding { return ls.bindings(run, pin) },
		ConsentKeyRevoked: func(id string) {
			if _, err := linkio.DropConsentKey(ls.stateDir, name, id); err != nil {
				say(ls.stderr, "link %s: drop consent key: %v", name, err)
			}
			if run.carrier != nil {
				run.carrier.RefreshConsentKeys()
			}
		},
		Revoked: func() { ls.retire(name) },
		Handle:  func(cmd *protocol.Command, src core.Source) (any, error) { return ls.handle(cmd, src) },
		ConsentKeys: func() []linkio.ConsentKey {
			keys, err := linkio.LoadConsentKeys(ls.stateDir, name)
			if err != nil {
				say(ls.stderr, "link %s: read consent keys: %v", name, err)
			}
			return keys
		},
		Logf: func(f string, a ...any) { say(ls.stderr, f, a...) },
	})
	if err != nil {
		return nil, err
	}
	run.carrier = c
	return run, nil
}

// consentLive is core's handoff check: the link with that creator host still
// accepts the consent key. A retired or unknown link accepts none.
func (ls *linkSet) consentLive(host, credential string) bool {
	ls.mu.Lock()
	c := ls.byHost[host]
	ls.mu.Unlock()
	return c != nil && c.ConsentLive(credential)
}

// handle runs a link's admitted command on the endpoint.
func (ls *linkSet) handle(cmd *protocol.Command, src core.Source) (any, error) {
	ls.mu.Lock()
	ep := ls.ep
	ls.mu.Unlock()
	if ep == nil {
		return nil, protocol.Refuse(protocol.CodeDraining, "the endpoint is not started")
	}
	return ep.Handle(cmd, src)
}

// retire makes a revoked link's sink history.
func (ls *linkSet) retire(name string) {
	host, err := linkio.Retire(ls.stateDir, name)
	if err != nil {
		say(ls.stderr, "link %s: retire: %v", name, err)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if host != "" {
		ls.retired[host] = true
		delete(ls.byHost, host)
	}
	delete(ls.running, name)
	writeLinkStatus(ls.stateDir, name, linkio.Status{State: "revoked"})
}

// bindings are the link's shares as this machine sees them now. A share is
// listed only while its binding names this root and its pinned native
// session is the one attached: a different session is never shared by
// inheritance. Answering capabilities are masked: a server never answers
// the agent's prompts.
func (ls *linkSet) bindings(run *linkRun, pin linkio.Pin) []linkio.Binding {
	ls.mu.Lock()
	ep, link := ls.ep, run.link
	ls.mu.Unlock()
	if ep == nil {
		return nil
	}
	sessions := map[string]protocol.Session{}
	for _, s := range ep.Sessions() {
		sessions[s.TargetID] = s
	}
	var out []linkio.Binding
	for _, sh := range link.Shares {
		b, err := binding.ReadNamed(sh.Binding)
		if err != nil || b.Mailbox() || filepath.Clean(b.Root) != filepath.Clean(ls.root) {
			continue
		}
		s, ok := sessions[b.Target]
		if !ok || ep.NativeSessionID(b.Target) != b.NativeSession {
			continue
		}
		caps := s.Capabilities
		caps.AnswerQuestion, caps.ApproveTool = false, false
		out = append(out, linkio.Binding{
			Binding: sh.Binding, TargetID: b.Target, Epoch: s.Epoch, NativeSessionID: b.NativeSession,
			Labels: linkio.Labels{
				Device: pin.DeviceName, Session: nonEmpty(b.Display, nonEmpty(s.DisplayName, b.Target)),
				Harness: s.Harness, Project: s.Project,
			},
			Harness: s.Harness, DisplayName: nonEmpty(b.Display, s.DisplayName), Attachment: s.Attachment,
			Consent: sh.Consent, Tools: sh.Tools, Capabilities: caps,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Binding < out[j].Binding })
	return out
}

func writeLinkStatus(stateDir, name string, st linkio.Status) {
	dir := linkio.LinkDir(stateDir, name)
	if _, err := os.Stat(dir); err != nil {
		return // a retired link keeps no state dir
	}
	data, err := json.Marshal(st)
	if err == nil {
		_ = os.WriteFile(filepath.Join(dir, "status.json"), data, 0o600)
	}
}

// link dispatches `amq-remote link add|remove|status`.
func link(args []string, stdin io.Reader, stdout io.Writer, probe *jsonProbe) (any, int, error) {
	if len(args) == 0 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "link needs a subcommand: add, remove or status")
	}
	switch args[0] {
	case "add":
		return linkAdd(args[1:], stdin, stdout, probe)
	case "remove":
		return linkRemove(args[1:], stdout, probe)
	case "status":
		return linkStatus(args[1:], probe)
	}
	return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "unknown link subcommand %q", args[0])
}

// linkInfo is the server's public description, read before redeeming.
type linkInfo struct {
	ServerID string `json:"server_id"`
	LinkURL  string `json:"link_url"`
}

type redeemRequest struct {
	DeviceKey  string `json:"device_key"`
	DeviceName string `json:"device_name"`
	Code       string `json:"code"`
	TS         int64  `json:"ts"`
	Signature  string `json:"signature"`
}

type redeemReply struct {
	DeviceID    string              `json:"device_id"`
	ServerID    string              `json:"server_id"`
	User        string              `json:"user"`
	ConsentKeys []linkio.ConsentKey `json:"consent_keys"`
}

// linkAdd links this root to a server with a single-use code the server
// issued, and pins the consent passkey whose fingerprint the user types from
// the browser that created it. The server cannot add a consent key; only a
// command typed here, with a fingerprint typed here, can.
func linkAdd(args []string, stdin io.Reader, stdout io.Writer, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	code := fs.String("code", "", "the single-use link code the server showed")
	deviceName := fs.String("device-name", "", "this machine's name as the server shows it (default: the host name)")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 2 || *code == "" {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "usage: amq-remote link add NAME URL --code CODE")
	}
	name, base := pos[0], strings.TrimRight(pos[1], "/")
	if err := linkio.ValidName(name); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if err := validServerURL(base); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	if *deviceName == "" {
		*deviceName, _ = os.Hostname()
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("the server redirected; refusing")
	}}
	var info linkInfo
	if err := getJSON(client, base+"/api/v1/link/info", &info); err != nil {
		return nil, protocol.ExitActionRequired, fmt.Errorf("read the server's link info: %w", err)
	}
	if info.ServerID == "" || manifest.ValidSocketURL(info.LinkURL) != nil {
		return nil, protocol.ExitActionRequired, errors.New("the server's link info has no server id or no wss:// link url")
	}
	key, err := linkio.MintDeviceKey(stateDir, name)
	if err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	linked := false
	defer func() {
		if !linked {
			_ = os.RemoveAll(linkio.LinkDir(stateDir, name)) // never redeemed or not confirmed
		}
	}()
	ts := time.Now().Unix()
	req := redeemRequest{
		DeviceKey: b64url(key.SPKI), DeviceName: *deviceName, Code: *code, TS: ts,
		Signature: b64url(key.Sign(linkio.RedeemMessage(info.ServerID, *code, ts))),
	}
	var rep redeemReply
	if err := postJSON(client, base+"/api/v1/link/redeem", req, &rep); err != nil {
		return nil, protocol.ExitActionRequired, fmt.Errorf("redeem the code: %w", err)
	}
	switch {
	case rep.ServerID != info.ServerID:
		return nil, protocol.ExitActionRequired, fmt.Errorf("the server answered as %q after describing itself as %q", rep.ServerID, info.ServerID)
	case rep.DeviceID != key.Host():
		return nil, protocol.ExitActionRequired, fmt.Errorf("the server named this device %q; its key says %q", rep.DeviceID, key.Host())
	case len(rep.ConsentKeys) != 1:
		return nil, protocol.ExitActionRequired, fmt.Errorf("the server sent %d consent keys; linking pins exactly one", len(rep.ConsentKeys))
	}
	ck := rep.ConsentKeys[0]
	want, err := linkio.Fingerprint(ck)
	if err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	say(stdout, "Device key %s (0600, new)", filepath.Join(linkio.LinkDir(stateDir, name), "device.key"))
	say(stdout, "Type the passkey fingerprint shown in your browser:")
	typed, _ := bufio.NewReader(stdin).ReadString('\n')
	if !linkio.SameFingerprint(typed, want) {
		return nil, protocol.ExitActionRequired, errors.New("the fingerprint does not match the key the server sent; nothing was linked")
	}
	pin := linkio.Pin{URL: info.LinkURL, ServerID: info.ServerID, DeviceID: rep.DeviceID, User: rep.User,
		DeviceName: *deviceName, RPID: ck.RPID, Origin: ck.Origin}
	if err := linkio.WritePin(stateDir, name, pin); err != nil {
		return nil, protocol.ExitError, err
	}
	if err := linkio.WriteConsentKeys(stateDir, name, rep.ConsentKeys); err != nil {
		return nil, protocol.ExitError, err
	}
	if err := editManifest(stateDir, func(mf *manifest.File) {
		mf.SchemaVersion = manifest.RelaySchemaVersion
		for _, l := range mf.Links {
			if l.Name == name {
				return
			}
		}
		mf.Links = append(mf.Links, manifest.Link{Name: name, URL: info.LinkURL, Shares: []manifest.LinkShare{}})
	}); err != nil {
		return nil, protocol.ExitError, err
	}
	linked = true
	if c.json {
		return map[string]any{"linked": name, "server_id": info.ServerID, "user": rep.User, "sink": key.Host()}, 0, nil
	}
	say(stdout, "Matches. Linked to %s on %s.", rep.User, info.ServerID)
	say(stdout, "Share a session: in it, run  amq-remote attach --self --link %s", name)
	return nil, 0, nil
}

// validServerURL accepts https://, and http:// only to a loopback host.
func validServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("server url %q is not a URL", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && ((ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost") {
		return nil
	}
	return fmt.Errorf("server url %q: use https://", raw)
}

func getJSON(client *http.Client, u string, out any) error {
	resp, err := client.Get(u)
	if err != nil {
		return err
	}
	return readJSONReply(resp, out)
}

func postJSON(client *http.Client, u string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := client.Post(u, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return readJSONReply(resp, out)
}

func readJSONReply(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// linkRemove retires a link's sink and deletes its device key, then drops the
// link from the manifest. A running endpoint stops the link on its next sweep.
func linkRemove(args []string, stdout io.Writer, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "usage: amq-remote link remove NAME")
	}
	name := pos[0]
	if err := linkio.ValidName(name); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	host, err := linkio.Retire(stateDir, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, protocol.ExitNotFound, protocol.Refuse(protocol.CodeNotFound, "no link named %q in this root", name)
	}
	if err != nil {
		return nil, protocol.ExitError, err
	}
	if err := dropManifestLink(stateDir, name); err != nil {
		return nil, protocol.ExitError, err
	}
	if !c.json {
		say(stdout, "Removed link %s. Sink %s is retired and its device key is deleted.", name, host)
		return nil, 0, nil
	}
	return map[string]any{"removed": name, "retired_sink": host}, 0, nil
}

// dropManifestLink removes one link from the manifest under the same lock
// attach uses for adapters.
func dropManifestLink(stateDir, name string) error {
	return editManifest(stateDir, func(mf *manifest.File) {
		kept := mf.Links[:0]
		for _, l := range mf.Links {
			if l.Name != name {
				kept = append(kept, l)
			}
		}
		mf.Links = kept
	})
}

// editManifest applies edit to the manifest under the lock attach uses.
func editManifest(stateDir string, edit func(*manifest.File)) error {
	if !lock.AdvisoryLockAvailable() {
		return errors.New("refusing to update the manifest without an advisory file lock")
	}
	path := manifest.DefaultPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(path+".lock", func() error {
		f, err := manifest.Load(path)
		if err != nil {
			return err
		}
		edit(&f)
		if err := manifest.Validate(f); err != nil {
			return err
		}
		return manifest.Write(path, f)
	})
}

// shareWithLink adds or replaces one binding's share on a declared link.
func shareWithLink(stateDir, name string, share manifest.LinkShare) error {
	found := false
	err := editManifest(stateDir, func(mf *manifest.File) {
		for i := range mf.Links {
			if mf.Links[i].Name != name {
				continue
			}
			found = true
			kept := mf.Links[i].Shares[:0]
			for _, sh := range mf.Links[i].Shares {
				if sh.Binding != share.Binding {
					kept = append(kept, sh)
				}
			}
			mf.Links[i].Shares = append(kept, share)
		}
	})
	if err == nil && !found {
		return fmt.Errorf("no link named %q in this root; link this machine first with amq-remote link add", name)
	}
	return err
}

// linkStatusOut is what `link status` prints.
type linkStatusOut struct {
	Links        []linkStatusRow `json:"links"`
	RetiredSinks []string        `json:"retired_sinks"`
}

func printLinkStatus(w io.Writer, v linkStatusOut) {
	if len(v.Links) == 0 {
		say(w, "No links. Link this machine with amq-remote link add.")
	}
	for _, l := range v.Links {
		say(w, "%s  %s  %s", l.Name, l.Status.State, l.URL)
		if l.Sink != "" {
			say(w, "  sink=%s gen=%d owed=%d", l.Sink, l.Status.Gen, l.Status.Owed)
		}
		if !l.Status.LastPing.IsZero() {
			say(w, "  last ping %s", l.Status.LastPing.Format(time.RFC3339))
		}
		if l.Status.Error != "" {
			say(w, "  %s", l.Status.Error)
		}
		say(w, "  shared: %v", l.Shares)
	}
	for _, h := range v.RetiredSinks {
		say(w, "retired %s: its records are abandoned on this link", h)
	}
}

type linkStatusRow struct {
	Name   string        `json:"name"`
	URL    string        `json:"url"`
	Sink   string        `json:"sink,omitempty"`
	Status linkio.Status `json:"status"`
	Shares []string      `json:"shares"`
}

// linkStatus lists the links of this root and the retired sinks.
func linkStatus(args []string, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	if _, err := parseInterleaved(fs, args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	mf, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil {
		return nil, protocol.ExitError, err
	}
	rows := []linkStatusRow{}
	for _, l := range mf.Links {
		row := linkStatusRow{Name: l.Name, URL: l.URL, Status: linkio.Status{State: "offline"}, Shares: []string{}}
		for _, sh := range l.Shares {
			row.Shares = append(row.Shares, sh.Binding)
		}
		if key, err := linkio.LoadDeviceKey(stateDir, l.Name); err == nil {
			row.Sink = key.Host()
		} else {
			row.Status.State = "not linked"
		}
		if data, err := os.ReadFile(filepath.Join(linkio.LinkDir(stateDir, l.Name), "status.json")); err == nil {
			_ = json.Unmarshal(data, &row.Status)
		}
		rows = append(rows, row)
	}
	retired, err := linkio.Retired(stateDir)
	if err != nil {
		return nil, protocol.ExitError, err
	}
	sinks := make([]string, 0, len(retired))
	for h := range retired {
		sinks = append(sinks, h)
	}
	sort.Strings(sinks)
	return linkStatusOut{Links: rows, RetiredSinks: sinks}, 0, nil
}
