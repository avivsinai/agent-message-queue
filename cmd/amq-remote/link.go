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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	karegistry "github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
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
	pages    *pageServer // the endpoint's own local pages (ruling cc)
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
		statuses: map[string]linkio.Status{}, pages: &pageServer{stateDir: stateDir},
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
		// The watchdog refreshes LastPing every few seconds; the file is
		// rewritten only when it moved by a minute or anything else changed.
		st.LastPing = st.LastPing.Truncate(time.Minute)
		if prev, ok := ls.statuses[r.link.Name]; !ok || prev != st {
			ls.statuses[r.link.Name] = st
			writeLinkStatus(ls.stateDir, r.link.Name, st)
		}
	}
}

// sync starts a carrier for each manifest link that has a device key and is
// not retired, and stops carriers whose link is gone. Disk reads happen
// outside the lock that core's publish path and every carrier's bindings
// wait on; only the map swap holds it.
func (ls *linkSet) sync(mf manifest.File) {
	ls.mu.Lock()
	ready := ls.ctx != nil
	ls.mu.Unlock()
	if !ready {
		return
	}
	retired, err := linkio.Retired(ls.stateDir)
	if err != nil {
		say(ls.stderr, "link: read retired sinks: %v", err)
		retired = nil
	}
	want := map[string]manifest.Link{}
	for _, l := range mf.Links {
		want[l.Name] = l
	}
	ls.mu.Lock()
	if retired != nil {
		ls.retired = retired
	}
	var missing []manifest.Link
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
		if ls.running[name] == nil {
			missing = append(missing, l)
		}
	}
	ls.mu.Unlock()
	for _, l := range missing {
		run, err := ls.newRun(l)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				say(ls.stderr, "link %s: %v", l.Name, err)
			}
			continue
		}
		ls.mu.Lock()
		if ls.running[l.Name] != nil || ls.retired[run.carrier.Host()] {
			ls.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(ls.ctx)
		run.cancel = cancel
		ls.running[l.Name] = run
		ls.byHost[run.carrier.Host()] = run.carrier
		ls.mu.Unlock()
		name := l.Name
		go func() {
			if err := run.carrier.Run(ctx); errors.Is(err, linkio.ErrRevoked) {
				say(ls.stderr, "link %s: the server revoked this device; the link is retired", name)
			}
		}()
		go ls.refreshToolProfiles(ctx, name)
	}
}

func (ls *linkSet) newRun(l manifest.Link) (*linkRun, error) {
	key, err := linkio.LoadDeviceKey(ls.stateDir, l.Name)
	if err != nil {
		return nil, err
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
		Name: name, Pin: pin, StoreID: storeID, Key: key, Version: ls.version,
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
		LocalKey: func() (linkio.ConsentKey, bool) {
			k, err := linkio.LoadLocalKey(ls.stateDir, name)
			return k, err == nil
		},
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
	if r := ls.running[name]; r != nil && r.cancel != nil {
		r.cancel()
	}
	delete(ls.running, name)
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

// writeLinkStatus records a link's status for `link status`, atomically so a
// reader never sees half a file.
func writeLinkStatus(stateDir, name string, st linkio.Status) {
	dir := linkio.LinkDir(stateDir, name)
	if _, err := os.Stat(dir); err != nil {
		return // a retired link keeps no state dir
	}
	if data, err := json.Marshal(st); err == nil {
		_, _ = fsq.WriteFileAtomic(dir, "status.json", data, 0o600)
	}
}

// link dispatches `amq-remote link add|remove|status`.
func link(args []string, stdin io.Reader, stdout io.Writer, probe *jsonProbe) (any, int, error) {
	if len(args) == 0 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "link needs a subcommand: add, keys, local-key, confirm, remove, status, tools or call")
	}
	switch args[0] {
	case "add":
		return linkAdd(args[1:], stdin, stdout, probe)
	case "confirm":
		return linkConfirm(args[1:], stdin, stdout, probe)
	case "local-key":
		return linkLocalKey(args[1:], stdin, stdout, probe)
	case "keys":
		if len(args) < 2 || args[1] != "add" {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "usage: amq-remote link keys add NAME --code CODE")
		}
		return linkKeysAdd(args[2:], stdin, stdout, probe)
	case "remove":
		return linkRemove(args[1:], stdout, probe)
	case "status":
		return linkStatus(args[1:], probe)
	case "tools":
		return linkTools(args[1:], probe)
	case "call":
		return linkCall(args[1:], os.Stderr, probe)
	}
	return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "unknown link subcommand %q", args[0])
}

// machineRoot is the per-machine root, ~/.amq/remote/root: the root a link
// lives in unless --root names another, and the one native attach uses
// outside AMQ (skills/amq-remote/SKILL.md).
func machineRoot() (string, error) {
	home, err := amqhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "remote", "root"), nil
}

// ensureMachineRoot creates the machine root, each directory 0700, and
// refuses a symlink or non-directory on the way.
func ensureMachineRoot() (string, error) {
	home, err := amqhome.EnsureDir()
	if err != nil {
		return "", err
	}
	dir := home
	for _, part := range []string{"remote", "root"} {
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s is not a plain directory; refusing", dir)
		}
	}
	return dir, nil
}

// sameRoot reports whether two roots name one directory, canonicalized the
// way up canonicalizes a root before it takes the root's lock.
func sameRoot(a, b string) bool {
	ca, errA := karegistry.CanonicalRoot(a)
	cb, errB := karegistry.CanonicalRoot(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ca == cb
}

// linkRoots are the roots a link command looks in: --root alone when given,
// otherwise the machine root and AM_ROOT when it is another directory.
func (c *common) linkRoots() ([]string, error) {
	if c.rootSet {
		if _, err := c.stateDir(); err != nil {
			return nil, err
		}
		return []string{c.root}, nil
	}
	m, err := machineRoot()
	if err != nil {
		return nil, err
	}
	roots := []string{m}
	if am := os.Getenv("AM_ROOT"); am != "" && filepath.IsAbs(am) && !sameRoot(am, m) {
		roots = append(roots, am)
	}
	return roots, nil
}

// rootLinks maps each link name declared in the roots' manifests to the
// roots that declare it. The manifest is the authority, not the device key,
// so a retired link is still found.
func rootLinks(roots []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, r := range roots {
		mf, err := manifest.Load(manifest.DefaultPath(filepath.Join(r, stateDirName)))
		if err != nil {
			return nil, err
		}
		for _, l := range mf.Links {
			if held := out[l.Name]; len(held) == 0 || held[len(held)-1] != r {
				out[l.Name] = append(held, r)
			}
		}
	}
	return out, nil
}

// resolveLink returns the root that holds link name ("" = the only link on
// this machine) and the link's name. A link lives in exactly one root; one
// name in two roots is refused, never guessed. --root names the one root
// searched. A refusal names the roots searched, because a link an earlier
// version added under another AM_ROOT is not on this machine's search path.
func (c *common) resolveLink(name string) (root, link string, err error) {
	roots, err := c.linkRoots()
	if err != nil {
		return "", "", err
	}
	held, err := rootLinks(roots)
	if err != nil {
		return "", "", err
	}
	where := strings.Join(roots, " or ")
	if name == "" {
		names := make([]string, 0, len(held))
		for n := range held {
			names = append(names, n)
		}
		sort.Strings(names)
		switch len(names) {
		case 0:
			return "", "", protocol.Refuse(protocol.CodeNotFound, "no link in %s; pass --root for a link in another root, or link it first with amq-remote link add NAME URL --code CODE", where)
		case 1:
			name = names[0]
		default:
			return "", "", protocol.Refuse(protocol.CodeInvalid, "%s has %d links (%s); name one with --link", where, len(names), strings.Join(names, ", "))
		}
	}
	switch rs := held[name]; len(rs) {
	case 0:
		return "", "", protocol.Refuse(protocol.CodeNotFound, "no link named %q in %s; pass --root for a link in another root, or link it first with amq-remote link add %s URL --code CODE", name, where, name)
	case 1:
		return rs[0], name, nil
	default:
		return "", "", protocol.Refuse(protocol.CodeUnsupported, "link %q exists in two roots, %s and %s; pass --root to pick one, or remove one with amq-remote link remove %s --root <root>", name, rs[0], rs[1], name)
	}
}

// requireLinkEndpoint refuses while an endpoint that predates links runs for
// this root: it would not serve the link, and its manifest writes (a live
// attach registers through it) would drop the links block. No endpoint at all
// is fine: the next one started is this binary.
func requireLinkEndpoint(stateDir string) error {
	f, running, err := ipc.EndpointFeatures(stateDir)
	switch {
	case err != nil:
		return err
	case running && !f.Link:
		return protocol.Refuse(protocol.CodeUnsupported, "the amq-remote endpoint running for this root is older than links: restart amq-remote up first")
	}
	return nil
}

// linkInfo is the server's public description, read before redeeming.
type linkInfo struct {
	ServerID string `json:"server_id"`
	LinkURL  string `json:"link_url"`
	// ToolProfiles are the tool profile names the server grants; nil when
	// the server does not say.
	ToolProfiles *[]string `json:"tool_profiles,omitempty"`
}

// grantedProfiles is the server's profile list with every name that is not
// a profile name dropped; nil stays nil (the server does not say).
func (i linkInfo) grantedProfiles() *[]string {
	if i.ToolProfiles == nil {
		return nil
	}
	kept := []string{}
	for _, p := range *i.ToolProfiles {
		if p != "" && manifest.ValidLinkShare(manifest.LinkShare{Binding: "session", Consent: "passkey", Tools: p}) == nil {
			kept = append(kept, p)
		}
	}
	return &kept
}

// linkHTTPClient reads a server's link API; it refuses redirects.
func linkHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("the server redirected; refusing")
	}}
}

// refreshToolProfiles rereads the tool profiles the server grants into the
// link's pin, where attach checks --tools. It runs once per link run, beside
// the carrier, so a slow server never holds up the link, and ends with the
// run. An answer from another server id, any error, or a link removed or
// added again meanwhile keeps the pin as it is.
func (ls *linkSet) refreshToolProfiles(ctx context.Context, name string) {
	pin, err := linkio.LoadPin(ls.stateDir, name)
	if err != nil || pin.API == "" {
		return
	}
	var info linkInfo
	if err := getJSONCtx(ctx, linkHTTPClient(), pin.API+"/api/v1/link/info", &info); err != nil {
		if ctx.Err() == nil {
			say(ls.stderr, "link %s: read the server's tool profiles: %v", name, err)
		}
		return
	}
	if info.ServerID != pin.ServerID {
		say(ls.stderr, "link %s: the server's link info names server %q, this link is pinned to %q; tool profiles kept", name, info.ServerID, pin.ServerID)
		return
	}
	// The link may have been removed and added again during the read: only
	// the same device on the same server takes the answer.
	now, err := linkio.LoadPin(ls.stateDir, name)
	if err != nil || now.DeviceID != pin.DeviceID || now.ServerID != pin.ServerID {
		return
	}
	now.ToolProfiles = info.grantedProfiles()
	if err := linkio.RewritePin(ls.stateDir, name, now); err != nil && !errors.Is(err, os.ErrNotExist) {
		say(ls.stderr, "link %s: record the server's tool profiles: %v", name, err)
	}
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

// linkAdd links this machine to a server with a single-use code the server
// issued, and pins the consent passkey whose fingerprint the user types from
// the browser that created it. The server cannot add a consent key; only a
// command typed here, with a fingerprint typed here, can. The link lives in
// the machine root unless --root names another; AM_ROOT does not choose it,
// because the terminal a user links from often exports an unrelated one.
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
	root := c.root
	if !c.rootSet {
		if root, err = machineRoot(); err != nil {
			return nil, protocol.ExitError, err
		}
	} else if _, err := c.stateDir(); err != nil {
		return nil, protocol.ExitUsage, err
	}
	// One name in two roots would be ambiguous for every later command; the
	// device key's O_EXCL guards only its own root.
	others := []string{}
	if m, err := machineRoot(); err == nil {
		others = append(others, m)
	}
	if am := os.Getenv("AM_ROOT"); am != "" && filepath.IsAbs(am) {
		others = append(others, am)
	}
	for _, other := range others {
		if sameRoot(other, root) {
			continue
		}
		held, err := rootLinks([]string{other})
		if err != nil {
			return nil, protocol.ExitError, err
		}
		if len(held[name]) > 0 {
			return nil, protocol.ExitActionRequired, protocol.Refuse(protocol.CodeUnsupported, "link %q already exists in %s; pick another name", name, other)
		}
	}
	if !c.rootSet {
		if root, err = ensureMachineRoot(); err != nil {
			return nil, protocol.ExitError, err
		}
	}
	stateDir := filepath.Join(root, stateDirName)
	if err := requireLinkEndpoint(stateDir); err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	if *deviceName == "" {
		*deviceName, _ = os.Hostname()
	}
	client := linkHTTPClient()
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
	say(stdout, "Device key %s (0600, new)", filepath.Join(linkio.LinkDir(stateDir, name), "device.key"))
	in := bufio.NewReader(stdin) // one reader: the fingerprint, then the y/N question
	rep, ck, err := redeemOneKey(client, base+"/api/v1/link/redeem", key, info.ServerID, *code, *deviceName, in, stdout)
	if errors.Is(err, errFingerprintMismatch) {
		return nil, protocol.ExitActionRequired, fmt.Errorf("%w; nothing was linked here. The server already registered device %s: unlink this machine on the server's connections page (%s), then run link add with a new code", err, key.Host(), base)
	}
	if err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	pin := linkio.Pin{API: base, URL: info.LinkURL, ServerID: info.ServerID, DeviceID: rep.DeviceID, User: rep.User,
		DeviceName: *deviceName, RPID: ck.RPID, Origin: ck.Origin, ToolProfiles: info.grantedProfiles()}
	if err := linkio.WritePin(stateDir, name, pin); err != nil {
		return nil, protocol.ExitError, err
	}
	if err := linkio.WriteConsentKeys(stateDir, name, rep.ConsentKeys); err != nil {
		return nil, protocol.ExitError, err
	}
	if err := editManifest(stateDir, func(mf *manifest.File) {
		mf.SchemaVersion = manifest.RelaySchemaVersion
		for i := range mf.Links {
			if mf.Links[i].Name == name {
				mf.Links[i].URL = info.LinkURL // a link added again keeps its shares, takes the server's URL
				return
			}
		}
		mf.Links = append(mf.Links, manifest.Link{Name: name, URL: info.LinkURL, Shares: []manifest.LinkShare{}})
	}); err != nil {
		return nil, protocol.ExitError, err
	}
	linked = true
	// The endpoint dials the link and serves link tools and mcp, so the
	// commands printed below work at once. A link that already runs an
	// endpoint picks the new link up on its next sweep.
	var startErr error
	if _, running, err := ipc.EndpointFeatures(stateDir); err == nil && !running {
		startErr = startEndpoint(root, stateDir)
	}
	if c.json {
		out := map[string]any{"linked": name, "server_id": info.ServerID, "user": rep.User, "sink": key.Host(), "root": root}
		if startErr != nil {
			out["endpoint_error"] = startErr.Error()
		}
		return out, 0, nil
	}
	say(stdout, "Matches. Linked to %s on %s.", rep.User, info.ServerID)
	if startErr != nil {
		say(stdout, "The endpoint did not start (%v); start it with amq-remote up --root %s", startErr, root)
	}
	if stdinIsTerminal(stdin) {
		offerLocalKey(stateDir, name, in, stdin, stdout)
	}
	rootArg := ""
	if m, err := machineRoot(); err != nil || !sameRoot(root, m) {
		rootArg = " --root " + root
	}
	say(stdout, "Share a session: in it, run  amq-remote attach --self --link %s%s", name, rootArg)
	say(stdout, "Use its tools here:      amq-remote link tools --link %s%s", name, rootArg)
	say(stdout, "Add them to Claude Code: claude mcp add %s -- amq-remote mcp --link %s%s", name, name, rootArg)
	return nil, 0, nil
}

// errFingerprintMismatch: the fingerprint the user typed is not the one of
// the key the server sent.
var errFingerprintMismatch = errors.New("the fingerprint does not match the key the server sent")

// redeemOneKey redeems a code with the device key's signature over the
// pinned server id, checks that the server answered as that server, for this
// device, with exactly one consent key, and accepts the key only when the
// fingerprint the user types from the browser matches it.
func redeemOneKey(client *http.Client, endpoint string, key linkio.DeviceKey, serverID, code, deviceName string, stdin io.Reader, stdout io.Writer) (redeemReply, linkio.ConsentKey, error) {
	ts := time.Now().Unix()
	req := redeemRequest{
		DeviceKey: b64url(key.SPKI), DeviceName: deviceName, Code: code, TS: ts,
		Signature: b64url(key.Sign(linkio.RedeemMessage(serverID, code, ts))),
	}
	var rep redeemReply
	if err := postJSON(client, endpoint, req, &rep); err != nil {
		return rep, linkio.ConsentKey{}, fmt.Errorf("redeem the code: %w", err)
	}
	switch {
	case rep.ServerID != serverID:
		return rep, linkio.ConsentKey{}, fmt.Errorf("the server answered as %q, this link is pinned to %q", rep.ServerID, serverID)
	case rep.DeviceID != key.Host():
		return rep, linkio.ConsentKey{}, fmt.Errorf("the server named this device %q; its key says %q", rep.DeviceID, key.Host())
	case len(rep.ConsentKeys) != 1:
		return rep, linkio.ConsentKey{}, fmt.Errorf("the server sent %d consent keys; a code binds exactly one", len(rep.ConsentKeys))
	}
	ck := rep.ConsentKeys[0]
	want, err := linkio.Fingerprint(ck)
	if err != nil {
		return rep, ck, err
	}
	say(stdout, "Type the passkey fingerprint shown in your browser:")
	typed, _ := bufio.NewReader(stdin).ReadString('\n')
	if !linkio.SameFingerprint(typed, want) {
		return rep, ck, errFingerprintMismatch
	}
	return rep, ck, nil
}

// linkKeysAdd adds a consent passkey to a linked machine with a code the
// server issued for it. Like linking, it accepts the key only with the
// fingerprint the user types here; the server cannot add a key by itself.
func linkKeysAdd(args []string, stdin io.Reader, stdout io.Writer, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link keys add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	code := fs.String("code", "", "the single-use code the server showed for the new passkey")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 || *code == "" {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "usage: amq-remote link keys add NAME --code CODE")
	}
	name := pos[0]
	if err := linkio.ValidName(name); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	root, _, err := c.resolveLink(name)
	if err != nil {
		return nil, protocol.ExitCode(err), err
	}
	stateDir := filepath.Join(root, stateDirName)
	key, err := linkio.LoadDeviceKey(stateDir, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, protocol.ExitNotFound, protocol.Refuse(protocol.CodeNotFound, "no link named %q in %s", name, root)
	}
	if err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	pin, err := linkio.LoadPin(stateDir, name)
	if err != nil || pin.API == "" {
		return nil, protocol.ExitActionRequired, fmt.Errorf("link %q has no server address; remove it and run link add again", name)
	}
	keys, err := linkio.LoadConsentKeys(stateDir, name)
	if err != nil {
		return nil, protocol.ExitError, err
	}
	_, ck, err := redeemOneKey(linkHTTPClient(), pin.API+"/api/v1/link/consent-keys/redeem", key, pin.ServerID, *code, pin.DeviceName, stdin, stdout)
	if err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	if ck.RPID != pin.RPID || ck.Origin != pin.Origin {
		return nil, protocol.ExitActionRequired, fmt.Errorf("the key is for %s, this link signs on %s; refused", ck.Origin, pin.Origin)
	}
	if slices.ContainsFunc(keys, func(k linkio.ConsentKey) bool { return k.CredentialID == ck.CredentialID }) {
		return nil, protocol.ExitActionRequired, fmt.Errorf("passkey %s is already accepted on link %q", ck.CredentialID, name)
	}
	if err := linkio.WriteConsentKeys(stateDir, name, append(keys, ck)); err != nil {
		return nil, protocol.ExitError, err
	}
	if c.json {
		return map[string]any{"link": name, "added": ck.CredentialID, "keys": len(keys) + 1}, 0, nil
	}
	say(stdout, "Matches. Link %s now accepts %d consent passkey(s).", name, len(keys)+1)
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
	return getJSONCtx(context.Background(), client, u, out)
}

func getJSONCtx(ctx context.Context, client *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
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
// The server keeps the machine until it is unlinked there, so the message
// says where.
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
	root, _, err := c.resolveLink(name)
	if err != nil {
		return nil, protocol.ExitCode(err), err
	}
	stateDir := filepath.Join(root, stateDirName)
	pin, _ := linkio.LoadPin(stateDir, name) // read before Retire deletes the link dir
	host, err := linkio.Retire(stateDir, name)
	if errors.Is(err, os.ErrNotExist) {
		// No device key: either no such link, or the server revoked it (close
		// 4010) and the endpoint already retired its sink. The manifest entry
		// is all that is left; drop it.
		mf, merr := manifest.Load(manifest.DefaultPath(stateDir))
		if merr != nil {
			return nil, protocol.ExitError, merr
		}
		if !slices.ContainsFunc(mf.Links, func(l manifest.Link) bool { return l.Name == name }) {
			return nil, protocol.ExitNotFound, protocol.Refuse(protocol.CodeNotFound, "no link named %q in %s", name, root)
		}
		if err := dropManifestLink(stateDir, name); err != nil {
			return nil, protocol.ExitError, err
		}
		if c.json {
			return map[string]any{"removed": name, "already_retired": true}, 0, nil
		}
		say(stdout, "Removed link %s. Its sink was already retired: the server revoked this device.", name)
		return nil, 0, nil
	}
	if err != nil {
		return nil, protocol.ExitError, err
	}
	if err := dropManifestLink(stateDir, name); err != nil {
		return nil, protocol.ExitError, err
	}
	if !c.json {
		say(stdout, "Removed link %s. Sink %s is retired and its device key is deleted.", name, host)
		if pin.API != "" {
			say(stdout, "This machine stays on the server until you unlink it on the server's connections page (%s).", pin.API)
		}
		return nil, 0, nil
	}
	out := map[string]any{"removed": name, "retired_sink": host}
	if pin.API != "" {
		out["server"] = pin.API
	}
	return out, 0, nil
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
		return fmt.Errorf("no link named %q on this machine; link this machine first with amq-remote link add", name)
	}
	return err
}

// linkStatusOut is what `link status` prints.
type linkStatusOut struct {
	Links        []linkStatusRow `json:"links"`
	RetiredSinks []retiredSink   `json:"retired_sinks"`
}

// retiredSink is a sink that will never dial again, and the link it served.
type retiredSink struct {
	Sink string `json:"sink"`
	Link string `json:"link"`
}

// retiredHostOf returns a retired sink that served the link name, or "".
func retiredHostOf(retired map[string]string, name string) string {
	for h, n := range retired {
		if n == name {
			return h
		}
	}
	return ""
}

func printLinkStatus(w io.Writer, v linkStatusOut) {
	if len(v.Links) == 0 {
		say(w, "No links. Link this machine with amq-remote link add.")
	}
	machine, _ := machineRoot()
	for _, l := range v.Links {
		say(w, "%s  %s  %s", l.Name, l.Status.State, l.URL)
		if machine == "" || !sameRoot(l.Root, machine) {
			say(w, "  root %s", l.Root)
		}
		if l.Sink != "" {
			say(w, "  sink=%s gen=%d owed=%d conflicts=%d", l.Sink, l.Status.Gen, l.Status.Owed, l.Status.Conflicts)
		}
		if !l.Status.LastPing.IsZero() {
			say(w, "  last ping %s", l.Status.LastPing.Format(time.RFC3339))
		}
		if l.Status.Error != "" {
			say(w, "  %s", l.Status.Error)
		}
		if len(l.Shares) == 0 {
			say(w, "  shared: none")
		}
		for _, b := range l.Shares {
			say(w, "  %s: %s", b, nonEmpty(l.ShareStates[b], "unknown"))
		}
	}
	for _, r := range v.RetiredSinks {
		say(w, "retired %s (link %s): abandoned; its records settle without network", r.Sink, r.Link)
	}
}

type linkStatusRow struct {
	Name   string        `json:"name"`
	Root   string        `json:"root"`
	URL    string        `json:"url"`
	Sink   string        `json:"sink,omitempty"`
	Status linkio.Status `json:"status"`
	Shares []string      `json:"shares"`
	// ShareStates says, per shared binding, whether the server sees it now:
	// "shared", or why not, with the step that fixes it.
	ShareStates map[string]string `json:"share_states,omitempty"`
}

// linkShareState says whether the server sees a shared binding now. A share is
// sent only while its binding names this root and its pinned native session
// is the one attached; otherwise it silently drops off the link, so the
// reason and the fix are stated here.
func linkShareState(root string, p *nativeProbe, name string) string {
	b, err := binding.ReadNamed(name)
	switch {
	case errors.Is(err, binding.ErrNone):
		return "no such binding: run amq-remote attach --self --link in the session to share"
	case err != nil:
		return "the binding cannot be read: " + err.Error()
	case b.Mailbox():
		return "a mailbox binding cannot be shared with a link"
	case filepath.Clean(b.Root) != filepath.Clean(root):
		return "the binding belongs to another root"
	}
	native, err := p.native(b.Target)
	switch {
	case err != nil:
		return "not attached now (" + err.Error() + ")"
	case native != b.NativeSession:
		return "session changed: run amq-remote attach --self --link again in the new session"
	}
	return "shared"
}

// nativeProbe asks the endpoint for a target's native session once per
// target, and stops asking after the first failure to reach it, so link
// status waits for at most one endpoint timeout however many shares it has.
type nativeProbe struct {
	stateDir string
	seen     map[string]string
	down     error
}

func (p *nativeProbe) native(target string) (string, error) {
	if p.down != nil {
		return "", p.down
	}
	if n, ok := p.seen[target]; ok {
		return n, nil
	}
	resp, err := ipc.Call(p.stateDir, ipc.Request{Native: &ipc.NativeQuery{TargetID: target}})
	if err != nil {
		p.down = err
		if isEndpointUnreachable(err) {
			p.down = errors.New("endpoint not running: run amq-remote up")
		}
		return "", p.down
	}
	if err := resp.AsError(); err != nil {
		return "", err
	}
	var reply ipc.NativeReply
	if err := json.Unmarshal(resp.Reply, &reply); err != nil || reply.NativeSession == "" {
		return "", fmt.Errorf("target %s reports no native session", target)
	}
	if p.seen == nil {
		p.seen = map[string]string{}
	}
	p.seen[target] = reply.NativeSession
	return reply.NativeSession, nil
}

// linkStatus lists the links on this machine (or in --root) and the retired
// sinks.
func linkStatus(args []string, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	if _, err := parseInterleaved(fs, args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	roots, err := c.linkRoots()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	rows := []linkStatusRow{}
	sinks := []retiredSink{}
	for _, root := range roots {
		stateDir := filepath.Join(root, stateDirName)
		mf, err := manifest.Load(manifest.DefaultPath(stateDir))
		if err != nil {
			return nil, protocol.ExitError, err
		}
		retiredNames, err := linkio.RetiredLinks(stateDir)
		if err != nil {
			return nil, protocol.ExitError, err
		}
		natives := &nativeProbe{stateDir: stateDir}
		for _, l := range mf.Links {
			row := linkStatusRow{Name: l.Name, Root: root, URL: l.URL, Status: linkio.Status{State: "offline"}, Shares: []string{}}
			for _, sh := range l.Shares {
				row.Shares = append(row.Shares, sh.Binding)
				if row.ShareStates == nil {
					row.ShareStates = map[string]string{}
				}
				row.ShareStates[sh.Binding] = linkShareState(root, natives, sh.Binding)
			}
			if key, err := linkio.LoadDeviceKey(stateDir, l.Name); err == nil {
				row.Sink = key.Host()
			} else if host := retiredHostOf(retiredNames, l.Name); host != "" {
				row.Sink, row.Status.State = host, "retired"
			} else {
				row.Status.State = "not linked"
			}
			if data, err := os.ReadFile(filepath.Join(linkio.LinkDir(stateDir, l.Name), "status.json")); err == nil {
				_ = json.Unmarshal(data, &row.Status)
			}
			rows = append(rows, row)
		}
		for h, n := range retiredNames {
			sinks = append(sinks, retiredSink{Sink: h, Link: n})
		}
	}
	sort.Slice(sinks, func(i, j int) bool { return sinks[i].Sink < sinks[j].Sink })
	return linkStatusOut{Links: rows, RetiredSinks: sinks}, 0, nil
}
