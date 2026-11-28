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
	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// memGrants is a GrantStore in memory. Generation hands out gen to a
// binding that has none; Mint writes mint, after minting closes when set,
// or fails with mintErr when set. Load fails with loadErr when set, and
// Clear with clearErr.
type memGrants struct {
	mu       sync.Mutex
	g        Grants
	gen      string
	mint     string
	minting  chan struct{}
	mintErr  error
	loadErr  error
	clearErr error
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
	if m.mintErr != nil {
		return "", m.mintErr
	}
	m.g[binding] = GrantRecord{Generation: m.mint}
	return m.mint, nil
}

func (m *memGrants) Clear(binding, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clearErr != nil {
		return m.clearErr
	}
	if rec := m.g[binding]; rec.Grant != nil && rec.Grant.Digest == digest {
		rec.Grant = nil
		m.g[binding] = rec
	}
	return nil
}

func (m *memGrants) Revoke(binding, digest string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clearErr != nil {
		return false, m.clearErr
	}
	rec := m.g[binding]
	if rec.Grant == nil || rec.Grant.Digest != digest {
		return false, nil
	}
	rec.Grant = nil
	m.g[binding] = rec
	return true, nil
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

// goldenGrant is the golden grant as stored for binding b under generation
// gen, ending at notAfter, signed by key and accepted under the key list of
// keys.
func goldenGrant(t *testing.T, b Binding, key ConsentKey, gen, notAfter string, keys ...ConsentKey) *StoredGrant {
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.CredentialID)
	}
	return &StoredGrant{
		GrantID: "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Digest: goldenGrantDigest(t), CredentialID: key.CredentialID,
		Binding: b.Binding, TargetID: b.TargetID, Epoch: b.Epoch, NativeSessionID: b.NativeSessionID, Labels: b.Labels,
		Generation: gen, IssuedAt: "2026-10-09T14:10:00Z", NotAfter: notAfter, MinEvidence: protocol.EvidenceSubmitted,
		KeyList: KeyList(ids),
	}
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
	store := &memGrants{mint: "g_5p2r6t3v7x4z2b5d6f3h7j4l2n", g: Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T15:10:00Z", key)}}}
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
// next granted task run), also when the last read of the store failed
// (review of the session grant branch: a revoke that marked the generation
// it found in an empty snapshot stopped nothing).
func TestGrantedSubmitAfterGrantRevokeIsRefused(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	minting := make(chan struct{})
	store := &memGrants{minting: minting, mint: "g_5p2r6t3v7x4z2b5d6f3h7j4l2n", g: Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key)}}}
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
	store.setLoadErr(errors.New("grants.json: input/output error"))
	c.RefreshGrants()
	store.setLoadErr(nil)
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

// Contract section 4, the key list: a grant signed by one key ends when
// the server revokes another key of the link, from the next frame on, and
// stays ended after a restart on the same state directory, where the
// machine cleared it (review of the session grant branch: the end hung on
// a generation mint after the drop, which could be lost). The same signed
// grant presented again is refused too, not answered as the stored grant
// (review of the key-list change: the stored grant's shortcut kept it). The
// hand-over check refuses it between the key_revoked frame and the drop of
// the key on disk (review of the key-list end: no row held that window).
// After the restart the same signed grant is grant_stale and no task runs
// under it: the cleared grant's issued_at stays as the binding's floor
// (review of the key-list end: an end kept only in memory let the same
// signed bytes back in after a restart).
func TestRevokedKeyEndsTheGrantOfAnotherKey(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g" // the golden grant's generation
	other := ConsentKey{CredentialID: "cred-revoked"}
	stateDir := t.TempDir()
	if err := WriteConsentKeys(stateDir, "example", []ConsentKey{key, other}); err != nil {
		t.Fatal(err)
	}
	stored := Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key, other)}}
	if err := writeJSON(LinkDir(stateDir, "example"), grantsFile, stored); err != nil {
		t.Fatal(err)
	}
	files := func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return LoadConsentKeys(stateDir, "example") }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = FileGrants(stateDir, "example")
		cfg.GrantInfoFor = func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes }
		cfg.Handle = func(*protocol.Command, core.Source) (any, error) {
			t.Error("a granted task ran after its link's key list changed")
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	}
	dropped := make(chan error, 1)
	revoking, drop := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(drop) })
	defer release()
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, func(cfg *Config) {
		files(cfg)
		cfg.ConsentKeyRevoked = func(id string) {
			close(revoking)
			<-drop
			_, err := DropConsentKey(stateDir, "example", id)
			dropped <- err
		}
	})
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	body, _ := json.Marshal(keyRevokedBody{Schema: SchemaKeyRevoked, CredentialID: other.CredentialID})
	fs.send(Frame{Schema: SchemaFrame, ID: "m_k1", Gen: wgen, Body: body})
	select {
	case <-revoking:
	case <-time.After(5 * time.Second):
		t.Fatal("the key revoke never reached the revoke worker")
	}
	if _, reason := c.GrantLive(b.TargetID, goldenGrantDigest(t)); !strings.HasPrefix(reason, CodeGrantRevoked+": ") {
		t.Fatalf("hand-over check before the revoked key is dropped on disk = %q, want %s", reason, CodeGrantRevoked)
	}
	release()
	for _, file := range []string{"49-granted_submit.json", "44-signed_grant.json"} {
		f := goldenFrame(t, file)
		f.Gen = wgen
		fs.send(f)
		var refused struct{ Error ErrorBody }
		if r := fs.next(); json.Unmarshal(r.Body, &refused) != nil || refused.Error.Code != CodeGrantRevoked || !strings.HasPrefix(refused.Error.Message, core.GrantKeysGone) {
			t.Fatalf("%s after the key revoke: %s; want %s for the key list", file, r.Body, CodeGrantRevoked)
		}
	}
	// The machine cleared the ended grant from its file and kept the
	// binding's generation (contract section 4: the end is permanent).
	if g, err := LoadGrants(stateDir, "example"); err != nil || g[b.Binding].Grant != nil || g[b.Binding].Generation != gen {
		t.Fatalf("grants.json after the key revoke: %+v, %v; want no grant and generation %s", g[b.Binding], err, gen)
	}
	select {
	case err := <-dropped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked key was never dropped")
	}
	devKey, err := MintDeviceKey(t.TempDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: "example", Pin: Pin{URL: "wss://link.example.invalid/v1", ServerID: testServerID}, Key: devKey, Now: clk.now}
	files(&cfg)
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := restarted.GrantLive(b.TargetID, goldenGrantDigest(t)); !strings.HasPrefix(reason, CodeGrantRevoked+": ") {
		t.Fatalf("hand-over check after a restart = %q, want %s", reason, CodeGrantRevoked)
	}
	if _, err := restarted.acceptGrant(goldenSignedGrant(t), b.Binding); refusalCode(err) != CodeGrantStale {
		t.Fatalf("the same signed grant after a restart: %v; want %s", err, CodeGrantStale)
	}
	if _, err := restarted.admitGranted(goldenGrantDigest(t), goldenGrantedCommand(t)); err == nil {
		t.Fatal("a granted task was admitted after a restart under the grant the key list ended")
	}
}

// olderSignedGrant is a grant for binding b in generation gen, issued 20 s
// before now for one hour and signed by key, and its digest.
func olderSignedGrant(t *testing.T, view *ConsentView, key ConsentKey, b Binding, gen string, now time.Time) (SignedGrant, string) {
	t.Helper()
	issued := now.Add(-20 * time.Second)
	doc, err := jcs.Marshal(GrantDocument{
		Schema: SchemaGrant, ServerID: view.ServerID, StoreID: view.StoreID,
		GrantID: "7f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Binding: b.Binding, TargetID: b.TargetID,
		Epoch: b.Epoch, NativeSessionID: b.NativeSessionID, Labels: b.Labels, GrantGeneration: gen,
		IssuedAt: issued.UTC().Format(time.RFC3339), NotAfter: issued.Add(time.Hour).UTC().Format(time.RFC3339),
		Input: GrantInput{Busy: "reject", Deliver: "turn", MinEvidence: "submitted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := SignedGrant(signDoc(t, key, doc))
	m.Schema = SchemaSignedGrant
	sum := sha256.Sum256(doc)
	return m, "sha256:" + hex.EncodeToString(sum[:])
}

// goldenSignedGrant is the signed grant of the golden frame 44.
func goldenSignedGrant(t *testing.T) SignedGrant {
	var m SignedGrant
	if err := json.Unmarshal(goldenFrame(t, "44-signed_grant.json").Body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// goldenGrantedCommand is the command of the golden frame 49.
func goldenGrantedCommand(t *testing.T) *protocol.Command {
	var m GrantedSubmit
	if err := json.Unmarshal(goldenFrame(t, "49-granted_submit.json").Body, &m); err != nil {
		t.Fatal(err)
	}
	cmd, err := protocol.DecodeCommand(m.Command)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// refusalCode is the code of a refusal, or "" for none.
func refusalCode(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// Contract section 4, the key list: the end is permanent. A grant ended by
// a key added to the link stays ended when the server then revokes that key
// and the list is back to the one the grant was accepted under: a granted
// task on the next frame, the hand-over check and the same signed grant
// presented again are refused, and the store holds no grant (review of the
// session grant branch: the grant was live again once the list returned).
func TestKeyListEndIsPermanent(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g" // the golden grant's generation
	added := ConsentKey{CredentialID: "cred-added"}
	var withAdded atomic.Bool
	store := &memGrants{g: Grants{b.Binding: {Generation: gen}}}
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) {
			if withAdded.Load() {
				return []ConsentKey{key, added}, nil
			}
			return []ConsentKey{key}, nil
		}
		cfg.ConsentKeyRevoked = func(string) { withAdded.Store(false) }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = store
		cfg.GrantInfoFor = func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes }
		cfg.Handle = func(*protocol.Command, core.Source) (any, error) {
			t.Error("a granted task ran after the key list ended its grant")
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
	sameBody(t, send("44-signed_grant.json"), "m_sg1", "45-grant_reply.json")
	withAdded.Store(true)
	c.Tick()
	body, _ := json.Marshal(keyRevokedBody{Schema: SchemaKeyRevoked, CredentialID: added.CredentialID})
	fs.send(Frame{Schema: SchemaFrame, ID: "m_k1", Gen: wgen, Body: body})
	for _, file := range []string{"49-granted_submit.json", "44-signed_grant.json"} {
		var refused struct{ Error ErrorBody }
		if r := send(file); json.Unmarshal(r.Body, &refused) != nil || refused.Error.Code != CodeGrantRevoked {
			t.Fatalf("%s with the key list back to the grant's: %s; want %s", file, r.Body, CodeGrantRevoked)
		}
	}
	if _, reason := c.GrantLive(b.TargetID, goldenGrantDigest(t)); !strings.HasPrefix(reason, CodeGrantRevoked+": ") {
		t.Fatalf("hand-over check with the key list back to the grant's = %q, want %s", reason, CodeGrantRevoked)
	}
	if g, _ := store.Load(); g[b.Binding].Grant != nil {
		t.Fatalf("the grant the key list ended is still stored: %+v", g[b.Binding].Grant)
	}
}

// Contract section 4, the key list: a grant the key list ended stays ended
// while the machine cannot clear it from its store. With every clear
// failing and the key list back to the one the grant was accepted under, a
// granted task and the hand-over check are refused grant_revoked (review of
// the key-list end: no row held an ended grant the store still held).
func TestKeyListEndHoldsWhenTheClearFails(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	var withAdded atomic.Bool
	store := &memGrants{clearErr: errors.New("grants.json: no space left on device"), g: Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key)}}}
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) {
			if withAdded.Load() {
				return []ConsentKey{key, {CredentialID: "cred-added"}}, nil
			}
			return []ConsentKey{key}, nil
		}
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = store
		cfg.GrantInfoFor = func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes }
		cfg.Handle = func(*protocol.Command, core.Source) (any, error) {
			t.Error("a granted task ran after the key list ended its grant")
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	})
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	withAdded.Store(true)
	c.Tick()
	withAdded.Store(false)
	c.Tick()
	if g, _ := store.Load(); g[b.Binding].Grant == nil {
		t.Fatal("the store lost the grant although every clear failed")
	}
	f := goldenFrame(t, "49-granted_submit.json")
	f.Gen = wgen
	fs.send(f)
	var refused struct{ Error ErrorBody }
	if r := fs.next(); json.Unmarshal(r.Body, &refused) != nil || refused.Error.Code != CodeGrantRevoked {
		t.Fatalf("granted_submit with the clear failing and the key list back: %s; want %s", r.Body, CodeGrantRevoked)
	}
	if _, reason := c.GrantLive(b.TargetID, goldenGrantDigest(t)); !strings.HasPrefix(reason, CodeGrantRevoked+": ") {
		t.Fatalf("hand-over check with the clear failing and the key list back = %q, want %s", reason, CodeGrantRevoked)
	}
}

// Contract section 4: the machine keeps the issued_at of the newest grant it
// accepted for a binding. A grant seen expired and cleared, presented again
// after the wall clock was set back into its 60 s window and the machine
// restarted, is grant_stale, and no task runs under it; a grant signed 20 s
// before it is grant_stale too, with no notice (review of the session grant
// branch: an end kept only in memory let the same or an older signed grant
// back in).
func TestEndedGrantIsStaleAfterARestart(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g" // the golden grant's generation
	stateDir := t.TempDir()
	if err := WriteConsentKeys(stateDir, "example", []ConsentKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(LinkDir(stateDir, "example"), grantsFile, Grants{b.Binding: {Generation: gen}}); err != nil {
		t.Fatal(err)
	}
	clk := grantClock()
	var mono atomic.Int64
	notices := make(chan string, 8)
	carrier := func() *Carrier {
		devKey, err := MintDeviceKey(t.TempDir(), "example")
		if err != nil {
			t.Fatal(err)
		}
		c, err := New(Config{
			Name: "example", Pin: Pin{URL: "wss://link.example.invalid/v1", ServerID: testServerID}, StoreID: view.StoreID, Key: devKey,
			Now: clk.now, Mono: func() time.Duration { return time.Duration(mono.Load()) },
			ConsentKeys:  func() ([]ConsentKey, error) { return LoadConsentKeys(stateDir, "example") },
			Bindings:     func() []Binding { return []Binding{b} },
			Grants:       FileGrants(stateDir, "example"),
			GrantInfoFor: func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes },
			Notify:       func(_, body string) { notices <- body },
			Handle: func(*protocol.Command, core.Source) (any, error) {
				t.Error("a granted task ran under a grant that ended")
				return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	notice := func(want string) {
		t.Helper()
		select {
		case n := <-notices:
			if !strings.Contains(n, want) {
				t.Fatalf("notice %q; want %q", n, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no notice %q", want)
		}
	}
	// An older grant: issued 20 s before the golden one, for the same binding.
	older, _ := olderSignedGrant(t, view, key, b, gen, clk.now())

	c := carrier()
	if _, err := c.acceptGrant(goldenSignedGrant(t), b.Binding); err != nil {
		t.Fatal(err)
	}
	notice("may send tasks")
	// The grant's 8 hours pass; the machine sees it expired and clears it.
	clk.advance(8 * time.Hour)
	mono.Store(int64(8 * time.Hour))
	c.Tick()
	notice("ended (expired)")
	if g, err := LoadGrants(stateDir, "example"); err != nil || g[b.Binding].Grant != nil {
		t.Fatalf("grants.json after the expiry: %+v, %v; want no grant", g[b.Binding], err)
	}
	// The wall clock is set back to 10 s after the golden grant's issued_at,
	// and the machine restarts.
	clk.advance(-8*time.Hour + 10*time.Second)
	c = carrier()
	for name, m := range map[string]SignedGrant{"the expired grant": goldenSignedGrant(t), "a grant signed before it": older} {
		if _, err := c.acceptGrant(m, b.Binding); refusalCode(err) != CodeGrantStale {
			t.Fatalf("%s presented after the restart: %v; want %s", name, err, CodeGrantStale)
		}
	}
	if _, err := c.admitGranted(goldenGrantDigest(t), goldenGrantedCommand(t)); err == nil {
		t.Fatal("a granted task was admitted under the expired grant after the restart")
	}
	select {
	case n := <-notices:
		t.Fatalf("notice %q for a grant presented after its end", n)
	case <-time.After(100 * time.Millisecond):
	}
}

// A grant_revoke whose new generation cannot be written answers
// storage_full, and the binding stays revoked: a granted task on the next
// frame is refused grant_revoked and the hand-over check refuses it (review
// of the session grant branch: no row held a binding revoked after its
// write failed).
func TestFailedRevokeWriteKeepsTheBindingRevoked(t *testing.T) {
	for _, stored := range []bool{true, false} {
		name := "a grant is stored"
		if !stored {
			name = "no grant is stored"
		}
		t.Run(name, func(t *testing.T) { failedRevokeWriteKeepsTheBindingRevoked(t, stored) })
	}
}

// With no grant stored the answer is still grant_revoked, not grant_unknown:
// the binding is in the revoked set.
func failedRevokeWriteKeepsTheBindingRevoked(t *testing.T, stored bool) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	rec := GrantRecord{Generation: gen}
	if stored {
		rec.Grant = goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key)
	}
	store := &memGrants{mintErr: errors.New("grants.json: no space left on device"), g: Grants{b.Binding: rec}}
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
	for _, step := range []struct{ file, code string }{
		{"55-grant_revoke.json", string(protocol.CodeStorageFull)},
		{"49-granted_submit.json", CodeGrantRevoked},
	} {
		f := goldenFrame(t, step.file)
		f.Gen = wgen
		fs.send(f)
		var refused struct{ Error ErrorBody }
		if r := fs.next(); json.Unmarshal(r.Body, &refused) != nil || refused.Error.Code != step.code {
			t.Fatalf("%s with the generation write failing: %s; want %s", step.file, r.Body, step.code)
		}
	}
	if _, reason := c.GrantLive(b.TargetID, goldenGrantDigest(t)); !strings.HasPrefix(reason, CodeGrantRevoked+": ") {
		t.Fatalf("hand-over check after the failed revoke write = %q, want %s", reason, CodeGrantRevoked)
	}
}

// Contract section 4, the key list: a key the CLI adds to the link ends its
// grants at the carrier's next read of the key list, and the owner is told
// (review of the session grant branch: the end hung on a generation mint
// beside the add).
func TestAddedKeyEndsTheGrant(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	stateDir := t.TempDir()
	if err := WriteConsentKeys(stateDir, "example", []ConsentKey{key}); err != nil {
		t.Fatal(err)
	}
	store := &memGrants{g: Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key)}}}
	devKey, err := MintDeviceKey(t.TempDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	notices := make(chan string, 4)
	c, err := New(Config{
		Name: "example", Pin: Pin{URL: "wss://link.example.invalid/v1", ServerID: testServerID}, StoreID: view.StoreID, Key: devKey, Now: grantClock().now,
		ConsentKeys: func() ([]ConsentKey, error) { return LoadConsentKeys(stateDir, "example") },
		Bindings:    func() []Binding { return []Binding{b} }, Grants: store,
		Notify: func(_, body string) { notices <- body },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := c.GrantLive(b.TargetID, goldenGrantDigest(t)); reason != "" {
		t.Fatalf("hand-over check before the add = %q, want live", reason)
	}
	if _, err := AddConsentKey(stateDir, "example", ConsentKey{CredentialID: "cred-added"}); err != nil {
		t.Fatal(err)
	}
	c.Tick()
	if _, reason := c.GrantLive(b.TargetID, goldenGrantDigest(t)); reason != CodeGrantRevoked+": "+core.GrantKeysGone {
		t.Fatalf("hand-over check after the add = %q, want the key list's end", reason)
	}
	select {
	case n := <-notices:
		if !strings.HasSuffix(n, "ended (revoked)") {
			t.Fatalf("notice %q; want the grant's end", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice when the added key ended the grant")
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
	store := &memGrants{mint: "g_5p2r6t3v7x4z2b5d6f3h7j4l2n", g: Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T15:10:00Z", key)}}}
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
		e, _ := c.endedGrant(digest)
		return e.code
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
	store := &memGrants{g: Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key)}}}
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
