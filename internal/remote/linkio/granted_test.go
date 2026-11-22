package linkio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// memGrants is a GrantStore in memory. Generation hands out gen to a
// binding that has none; Mint writes mint, after minting closes when set,
// and fails while failMints is above 0. Load fails with loadErr when set.
type memGrants struct {
	mu        sync.Mutex
	g         Grants
	gen       string
	mint      string
	minting   chan struct{}
	failMints int
	loadErr   error
}

func (m *memGrants) Load() (Grants, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	return maps.Clone(m.g), nil
}

func (m *memGrants) setLoadErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadErr = err
}

func (m *memGrants) Generation(binding string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.g[binding]
	if rec.Generation == "" {
		rec.Generation = m.gen
		m.g[binding] = rec
	}
	return rec.Generation, nil
}

func (m *memGrants) Store(sg StoredGrant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.g[sg.Binding]
	if rec.Generation != sg.Generation {
		return ErrGrantGeneration
	}
	rec.Grant = &sg
	m.g[sg.Binding] = rec
	return nil
}

func (m *memGrants) Mint(binding string) (string, error) {
	if m.minting != nil {
		<-m.minting
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failMints > 0 {
		m.failMints--
		return "", errors.New("grants.json: no space left on device")
	}
	m.g[binding] = GrantRecord{Generation: m.mint}
	return m.mint, nil
}

// goldenFrame is the frame of testdata/link/frames/<file>.
func goldenFrame(t *testing.T, file string) Frame {
	t.Helper()
	var f struct {
		Frame Frame `json:"frame"`
	}
	if err := json.Unmarshal(readLinkFixture(t, "frames/"+file), &f); err != nil {
		t.Fatal(err)
	}
	return f.Frame
}

// sameBody fails unless the endpoint's reply r answers id with the body of
// the golden frame file.
func sameBody(t *testing.T, r Frame, id, file string) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal(r.Body, &got); err != nil || json.Unmarshal(goldenFrame(t, file).Body, &want) != nil || r.Re != id || !reflect.DeepEqual(got, want) {
		t.Fatalf("reply to %s: %s; want the body of %s", id, r.Body, file)
	}
}

// grantClock is the machine's clock of the golden grant exchange.
func grantClock() *clock {
	clk := newClock()
	clk.ns.Store(time.Date(2026, 10, 9, 14, 10, 0, 0, time.UTC).UnixNano())
	return clk
}

// goldenGrantDigest is the digest of testdata/link/grant/document.json.
func goldenGrantDigest(t *testing.T) string {
	sum := sha256.Sum256(readLinkFixture(t, "grant/document.json"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Over the link: grant_info, the golden signed grant, then a granted task.
// The task reaches the endpoint with the grant's native session, signing
// key and digest, and every reply body is the contract's golden frame.
func TestGrantedSubmitRunsOverTheLink(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	store := &memGrants{g: Grants{}, gen: "g_3jx7k2m4qv5w6y7zab2cd3ef4g"}
	digest := goldenGrantDigest(t)
	fs, clk := newFakeServer(t), grantClock()
	ran := make(chan core.Source, 1)
	notices := make(chan string, 4)
	var c *Carrier
	c, _ = startCarrier(t, fs, clk, func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return []ConsentKey{key}, nil }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = store
		cfg.GrantInfoFor = func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes }
		cfg.Notify = func(_, body string) { notices <- body }
		cfg.Handle = func(cmd *protocol.Command, src core.Source) (any, error) {
			// Core asks the same grant at the hand-over.
			if binding, reason := c.GrantLive(cmd.TargetID, src.GrantDigest); binding != b.Binding || reason != "" {
				t.Errorf("hand-over check = %q, %q; want the live grant of %s", binding, reason, b.Binding)
			}
			ran <- src
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	})
	gen := fs.waitWelcome()
	waitOnline(t, c)
	send := func(file string) {
		f := goldenFrame(t, file)
		f.Gen = gen
		fs.send(f)
	}

	send("41-grant_info.json")
	sameBody(t, fs.next(), "m_gi1", "42-grant_info_reply.json")
	send("44-signed_grant.json")
	sameBody(t, fs.next(), "m_sg1", "45-grant_reply.json")
	select {
	case <-notices:
	case <-time.After(5 * time.Second):
		t.Fatal("no desktop notice for the accepted grant")
	}
	send("49-granted_submit.json")
	select {
	case src := <-ran:
		if src.GrantDigest != digest || src.NativeSession != b.NativeSessionID || src.Credential != key.CredentialID || src.Origin["carrier"] != core.CarrierLink {
			t.Fatalf("source %+v: want the link with the grant's native session, key and digest", src)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the granted task never reached the endpoint")
	}
	sameBody(t, fs.next(), "m_gs1", "50-granted_submit_outcome.json")
}

// Contract section 4, expiry is kept: inside one run, a wall clock set back
// after the grant was read extends nothing. The grant ends when the life it
// had left is used up on the monotonic clock, and the store ends it (review
// of the session grant branch: expiry compared only the wall clock).
func TestGrantExpiryIsKeptWhenTheWallClockGoesBack(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	digest := goldenGrantDigest(t)
	store := &memGrants{mint: "g_5p2r6t3v7x4z2b5d6f3h7j4l2n", g: Grants{b.Binding: {Generation: gen, Grant: &StoredGrant{
		GrantID: "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Digest: digest, CredentialID: key.CredentialID,
		Binding: b.Binding, TargetID: b.TargetID, Epoch: b.Epoch, NativeSessionID: b.NativeSessionID, Labels: b.Labels,
		Generation: gen, IssuedAt: "2026-10-09T14:10:00Z", NotAfter: "2026-10-09T15:10:00Z", MinEvidence: protocol.EvidenceSubmitted,
	}}}}
	devKey, err := MintDeviceKey(t.TempDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	var mono atomic.Int64
	c, err := New(Config{
		Name: "example", Pin: Pin{URL: "wss://link.example.invalid/v1", ServerID: testServerID}, StoreID: view.StoreID, Key: devKey,
		Now: grantClock().now, Mono: func() time.Duration { return time.Duration(mono.Load()) },
		ConsentKeys: func() ([]ConsentKey, error) { return []ConsentKey{key}, nil }, Bindings: func() []Binding { return []Binding{b} }, Grants: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := c.GrantLive(b.TargetID, digest); reason != "" {
		t.Fatalf("hand-over check of the loaded grant = %q, want live", reason)
	}
	// The grant's hour passes on the monotonic clock; the wall clock was set
	// back by as much and still reads the time the grant was loaded.
	mono.Store(int64(time.Hour))
	if _, reason := c.GrantLive(b.TargetID, digest); !strings.HasPrefix(reason, CodeGrantExpired+":") {
		t.Fatalf("hand-over check after its hour = %q, want %s", reason, CodeGrantExpired)
	}
	c.RefreshGrants()
	if g, _ := store.Load(); g[b.Binding].Grant != nil {
		t.Fatalf("the expired grant is still stored: %+v", g[b.Binding])
	}
}

// A granted_submit read right after a grant_revoke is refused grant_revoked,
// while the new generation is not yet written: the revoke counts from the
// next frame on (contract attack: a revoke that waited for the disk let the
// next granted task run).
func TestGrantedSubmitAfterGrantRevokeIsRefused(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	minting := make(chan struct{})
	store := &memGrants{minting: minting, mint: "g_5p2r6t3v7x4z2b5d6f3h7j4l2n", g: Grants{b.Binding: {Generation: gen, Grant: &StoredGrant{
		GrantID: "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Digest: goldenGrantDigest(t), CredentialID: key.CredentialID,
		Binding: b.Binding, TargetID: b.TargetID, Epoch: b.Epoch, NativeSessionID: b.NativeSessionID, Labels: b.Labels,
		Generation: gen, IssuedAt: "2026-10-09T14:10:00Z", NotAfter: "2026-10-09T22:10:00Z", MinEvidence: protocol.EvidenceSubmitted,
	}}}}
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return []ConsentKey{key}, nil }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = store
		cfg.GrantInfoFor = func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes }
		cfg.Handle = func(*protocol.Command, core.Source) (any, error) {
			t.Error("a granted task ran after its grant was revoked")
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	})
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	for _, file := range []string{"55-grant_revoke.json", "49-granted_submit.json"} {
		f := goldenFrame(t, file)
		f.Gen = wgen
		fs.send(f)
	}
	sameBody(t, fs.next(), "m_gs1", "53-granted_submit_refused_grant_revoked.json")
	// The hand-over check refuses it too, before the new generation is written.
	if _, reason := c.GrantLive(b.TargetID, goldenGrantDigest(t)); !strings.HasPrefix(reason, CodeGrantRevoked+":") {
		t.Fatalf("hand-over check after the revoke = %q, want %s", reason, CodeGrantRevoked)
	}
	close(minting)
	sameBody(t, fs.next(), "m_gr1", "56-revoked_reply.json")
}

// A dropped consent key ends every grant of the link from the moment of the
// drop, also a grant another key signed, and when the new generation cannot
// be written after the drop, the next tick writes it (review of the session
// grant branch: a failed mint after a drop left the other key's grants live).
func TestDroppedKeyEndsEveryGrantWhenItsMintFails(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	other := ConsentKey{CredentialID: "cred-dropped"}
	store := &memGrants{failMints: 1, mint: "g_5p2r6t3v7x4z2b5d6f3h7j4l2n", g: Grants{b.Binding: {Generation: gen, Grant: &StoredGrant{
		GrantID: "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Digest: goldenGrantDigest(t), CredentialID: key.CredentialID,
		Binding: b.Binding, TargetID: b.TargetID, Epoch: b.Epoch, NativeSessionID: b.NativeSessionID, Labels: b.Labels,
		Generation: gen, IssuedAt: "2026-10-09T14:10:00Z", NotAfter: "2026-10-09T22:10:00Z", MinEvidence: protocol.EvidenceSubmitted,
	}}}}
	minted := make(chan error, 1)
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return []ConsentKey{key, other}, nil }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = store
		cfg.GrantInfoFor = func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes }
		// serve drops the key from the list, then its mint fails.
		cfg.ConsentKeyRevoked = func(string) {
			_, err := store.Mint(b.Binding)
			minted <- err
		}
		cfg.Handle = func(*protocol.Command, core.Source) (any, error) {
			t.Error("a granted task ran after a key of its link was dropped")
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	})
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	body, _ := json.Marshal(keyRevokedBody{Schema: SchemaKeyRevoked, CredentialID: other.CredentialID})
	fs.send(Frame{Schema: SchemaFrame, ID: "m_k1", Gen: wgen, Body: body})
	f := goldenFrame(t, "49-granted_submit.json")
	f.Gen = wgen
	fs.send(f)
	sameBody(t, fs.next(), "m_gs1", "53-granted_submit_refused_grant_revoked.json")
	select {
	case err := <-minted:
		if err == nil {
			t.Fatal("the mint after the drop did not fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the key was never dropped")
	}
	if g, _ := store.Load(); g[b.Binding].Generation != gen {
		t.Fatalf("generation %q before the tick, want the old one still written", g[b.Binding].Generation)
	}
	c.Tick()
	if g, _ := store.Load(); g[b.Binding].Generation == gen || g[b.Binding].Grant != nil {
		t.Fatalf("after the tick the record is %+v; want a new generation and no grant", g[b.Binding])
	}
}

// A key list or a grant store that cannot be read ends no grant: no task
// runs under the grant meanwhile, but the owner is not told it ended and it
// is not counted as ended; the first end notice is the grant's real end
// (review of the session grant branch: a failed read of the key file told
// the owner a live grant was revoked).
func TestFailedReadEndsNoGrant(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	digest := goldenGrantDigest(t)
	store := &memGrants{mint: "g_5p2r6t3v7x4z2b5d6f3h7j4l2n", g: Grants{b.Binding: {Generation: gen, Grant: &StoredGrant{
		GrantID: "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Digest: digest, CredentialID: key.CredentialID,
		Binding: b.Binding, TargetID: b.TargetID, Epoch: b.Epoch, NativeSessionID: b.NativeSessionID, Labels: b.Labels,
		Generation: gen, IssuedAt: "2026-10-09T14:10:00Z", NotAfter: "2026-10-09T15:10:00Z", MinEvidence: protocol.EvidenceSubmitted,
	}}}}
	devKey, err := MintDeviceKey(t.TempDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	var keysFail atomic.Bool
	var mono atomic.Int64
	notices := make(chan string, 4)
	c, err := New(Config{
		Name: "example", Pin: Pin{URL: "wss://link.example.invalid/v1", ServerID: testServerID}, StoreID: view.StoreID, Key: devKey,
		Now: grantClock().now, Mono: func() time.Duration { return time.Duration(mono.Load()) },
		ConsentKeys: func() ([]ConsentKey, error) {
			if keysFail.Load() {
				return nil, errors.New("consent_keys.json: input/output error")
			}
			return []ConsentKey{key}, nil
		},
		Bindings: func() []Binding { return []Binding{b} }, Grants: store,
		Notify: func(_, body string) { notices <- body },
	})
	if err != nil {
		t.Fatal(err)
	}
	ended := func() string {
		c.gmu.Lock()
		defer c.gmu.Unlock()
		return c.grantEnded[digest]
	}
	keysFail.Store(true)
	c.RefreshConsentKeys()
	c.RefreshGrants()
	if _, reason := c.GrantLive(b.TargetID, digest); reason == "" {
		t.Fatal("hand-over check with the key list unread = live, want refused")
	}
	if code := ended(); code != "" {
		t.Fatalf("after a failed read of the key list the grant counts as ended (%s)", code)
	}
	keysFail.Store(false)
	c.RefreshConsentKeys()
	store.setLoadErr(errors.New("grants.json: input/output error"))
	c.RefreshGrants()
	if _, reason := c.GrantLive(b.TargetID, digest); reason == "" {
		t.Fatal("hand-over check with the store unread = live, want refused")
	}
	if code := ended(); code != "" {
		t.Fatalf("after a failed read of the store the grant counts as ended (%s)", code)
	}
	store.setLoadErr(nil)
	c.RefreshGrants()
	if _, reason := c.GrantLive(b.TargetID, digest); reason != "" {
		t.Fatalf("hand-over check after both reads recovered = %q, want live", reason)
	}
	mono.Store(int64(time.Hour))
	c.RefreshGrants()
	select {
	case n := <-notices:
		if !strings.HasSuffix(n, "ended (expired)") {
			t.Fatalf("first end notice %q; want the expiry, the grant's only end", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice when the grant expired")
	}
}

// Contract section 4: a session that can write the machine's state directory
// with no dialog takes no grant. grant_info answers available false with the
// reason state_in_workspace, and a task under a stored grant is refused
// grant_unsupported, not run (review of the session grant branch: a granted
// task could have the session rewrite the grant state).
func TestSessionThatWritesTheStateTakesNoGrant(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	store := &memGrants{g: Grants{b.Binding: {Generation: gen, Grant: &StoredGrant{
		GrantID: "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Digest: goldenGrantDigest(t), CredentialID: key.CredentialID,
		Binding: b.Binding, TargetID: b.TargetID, Epoch: b.Epoch, NativeSessionID: b.NativeSessionID, Labels: b.Labels,
		Generation: gen, IssuedAt: "2026-10-09T14:10:00Z", NotAfter: "2026-10-09T22:10:00Z", MinEvidence: protocol.EvidenceSubmitted,
	}}}}
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return []ConsentKey{key}, nil }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = store
		cfg.GrantInfoFor = func(Binding) (bool, string, int) { return false, GrantReasonStateInWorkspace, 0 }
		cfg.Handle = func(*protocol.Command, core.Source) (any, error) {
			t.Error("a granted task ran on a session that writes the grant state")
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	})
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	send := func(file string) Frame {
		f := goldenFrame(t, file)
		f.Gen = wgen
		fs.send(f)
		return fs.next()
	}
	var info GrantInfoReply
	if r := send("41-grant_info.json"); json.Unmarshal(r.Body, &info) != nil || info.GrantInfo.Available || info.GrantInfo.Reason != GrantReasonStateInWorkspace {
		t.Fatalf("grant_info reply %s; want available false with reason %s", r.Body, GrantReasonStateInWorkspace)
	}
	var refused struct{ Error ErrorBody }
	if r := send("49-granted_submit.json"); json.Unmarshal(r.Body, &refused) != nil || refused.Error.Code != CodeGrantUnsupported || !strings.HasPrefix(refused.Error.Message, GrantReasonStateInWorkspace+": ") {
		t.Fatalf("granted_submit reply %s; want %s with the reason", r.Body, CodeGrantUnsupported)
	}
}
