package linkio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// binding that has none; Mint writes mint, after minting closes when set.
type memGrants struct {
	mu      sync.Mutex
	g       Grants
	gen     string
	mint    string
	minting chan struct{}
}

func (m *memGrants) Load() (Grants, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.g), nil
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
		cfg.ConsentKeys = func() []ConsentKey { return []ConsentKey{key} }
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
		ConsentKeys: func() []ConsentKey { return []ConsentKey{key} }, Bindings: func() []Binding { return []Binding{b} }, Grants: store,
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
		cfg.ConsentKeys = func() []ConsentKey { return []ConsentKey{key} }
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
