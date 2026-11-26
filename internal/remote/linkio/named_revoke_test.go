package linkio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// namedRevokeFrame is the golden grant_revoke/2 frame, naming digest.
func namedRevokeFrame(t *testing.T, digest string) Frame {
	t.Helper()
	f := goldenFrame(t, "84-grant_revoke_v2.json")
	var body map[string]any
	if err := json.Unmarshal(f.Body, &body); err != nil {
		t.Fatal(err)
	}
	body["grant_digest"] = digest
	f.Body = mustJSON(body)
	return f
}

// namedRevokeStore is the grants.json of link "example" in a new state
// directory, holding rec for binding.
func namedRevokeStore(t *testing.T, binding string, rec GrantRecord) string {
	t.Helper()
	stateDir := t.TempDir()
	if err := writeJSON(LinkDir(stateDir, "example"), grantsFile, Grants{binding: rec}); err != nil {
		t.Fatal(err)
	}
	return stateDir
}

// grantGate holds the first grant availability check made after it is
// armed until it is released: the job that makes the check, and every later
// job of its binding, waits behind it.
type grantGate struct {
	armed            atomic.Bool
	waiting, release chan struct{}
}

func newGrantGate() *grantGate {
	return &grantGate{waiting: make(chan struct{}), release: make(chan struct{})}
}

func (g *grantGate) check() {
	if g.armed.CompareAndSwap(true, false) {
		close(g.waiting)
		<-g.release
	}
}

// holdOn arms g, sends f on fs and returns once f's job waits at g.
func (g *grantGate) holdOn(t *testing.T, fs *fakeServer, gen int64, f Frame) {
	t.Helper()
	g.armed.Store(true)
	f.Gen = gen
	fs.send(f)
	select {
	case <-g.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the held job never reached the machine")
	}
}

// namedRevokeMachine is the machine of the golden binding on store: its
// grant availability checks pass g, its notices go to notices, and no
// granted task may run.
func namedRevokeMachine(t *testing.T, store GrantStore, g *grantGate, notices chan string) func(*Config) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	return func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return []ConsentKey{key}, nil }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = store
		cfg.Notify = func(_, body string) {
			if notices != nil {
				notices <- body
			}
		}
		cfg.GrantInfoFor = func(Binding) (bool, string, int) {
			if g != nil {
				g.check()
			}
			return true, "", protocol.MaxInputBytes
		}
		cfg.Handle = func(*protocol.Command, core.Source) (any, error) {
			t.Error("a granted task ran under a grant a revoke named")
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	}
}

// review finding, 2026-11-26: a revoke of one grant could end its successor.
// Delayed A, live B: the server still holds the row of grant A when B,
// issued later for the same binding, is accepted, and only then sends a
// grant_revoke/2 naming A. The machine answers ended false and keeps B, its
// generation and its floor: a granted task under B runs, the hand-over check
// passes, and B is still on disk when the store is read again.
func TestNamedRevokeOfAReplacedGrantKeepsItsSuccessor(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g" // the golden grant's generation
	stateDir := namedRevokeStore(t, b.Binding, GrantRecord{Generation: gen})
	digestB := goldenGrantDigest(t)
	fs, clk := newFakeServer(t), grantClock()
	var c *Carrier
	c, _ = startCarrier(t, fs, clk, func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return []ConsentKey{key}, nil }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = FileGrants(stateDir, "example")
		cfg.GrantInfoFor = func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes }
		cfg.Handle = func(cmd *protocol.Command, src core.Source) (any, error) {
			if _, reason := c.GrantLive(cmd.TargetID, src.GrantDigest); reason != "" {
				t.Errorf("hand-over check of B = %q, want live", reason)
			}
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	})
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	send := func(f Frame) Frame {
		f.Gen = wgen
		fs.send(f)
		return fs.next()
	}
	older, digestA := olderSignedGrant(t, view, key, b, gen, clk.now())
	var accepted struct{ Grant GrantAccepted }
	if r := send(Frame{Schema: SchemaFrame, ID: "m_sga", Body: mustJSON(older)}); json.Unmarshal(r.Body, &accepted) != nil || accepted.Grant.Digest != digestA {
		t.Fatalf("signed grant A: %s; want it accepted", r.Body)
	}
	sameBody(t, send(goldenFrame(t, "44-signed_grant.json")), "m_sg1", "45-grant_reply.json")
	before, err := LoadGrants(stateDir, "example")
	if err != nil || before[b.Binding].Grant == nil || before[b.Binding].Grant.Digest != digestB {
		t.Fatalf("grants.json after B: %+v, %v; want B stored", before[b.Binding], err)
	}

	r := send(namedRevokeFrame(t, digestA))
	var revoked struct {
		Revoked struct {
			Binding     string `json:"binding"`
			GrantDigest string `json:"grant_digest"`
			Ended       *bool  `json:"ended"`
		} `json:"revoked"`
	}
	if r.Re != "m_gr2" || decodeStrict(r.Body, "", &revoked) != nil || revoked.Revoked.Binding != b.Binding ||
		revoked.Revoked.GrantDigest != digestA || revoked.Revoked.Ended == nil || *revoked.Revoked.Ended {
		t.Fatalf("grant_revoke/2 of A after B: %s; want ended false for A", r.Body)
	}
	if _, reason := c.GrantLive(b.TargetID, digestB); reason != "" {
		t.Fatalf("hand-over check of B after the revoke of A = %q, want live", reason)
	}
	sameBody(t, send(goldenFrame(t, "49-granted_submit.json")), "m_gs1", "50-granted_submit_outcome.json")
	after, err := LoadGrants(stateDir, "example")
	if err != nil || !reflect.DeepEqual(after[b.Binding], before[b.Binding]) {
		t.Fatalf("grants.json read again after the revoke of A: %+v, %v; want B, its generation and floor unchanged: %+v", after[b.Binding], err, before[b.Binding])
	}
}

// review finding, 2026-11-26: a revoke of one grant could end its successor.
// Matching B, then a retry: a grant_revoke/2 naming the live grant B waits
// behind another job of the binding, with the snapshot still showing B. A
// granted task under B and the hand-over check refuse B meanwhile, and B
// stays on disk. The answer, ended true, comes after the store cleared B and
// kept its generation and floor, with one end notice. The same revoke sent
// again to the restarted machine answers ended false and changes nothing,
// with no second notice, and the signed B presented again is refused.
func TestNamedRevokeHoldsTheGrantUntilTheStoreDecides(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	digestB := goldenGrantDigest(t)
	stateDir := namedRevokeStore(t, b.Binding, GrantRecord{Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key)})
	notices := make(chan string, 8)
	gate := newGrantGate()
	machine := namedRevokeMachine(t, FileGrants(stateDir, "example"), gate, notices)
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, machine)
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	sendOn := func(fs *fakeServer, gen int64, f Frame) {
		f.Gen = gen
		fs.send(f)
	}
	// A grant_info of the binding holds the binding's queue; the revoke of B
	// waits behind it.
	gate.holdOn(t, fs, wgen, goldenFrame(t, "41-grant_info.json"))
	sendOn(fs, wgen, namedRevokeFrame(t, digestB))
	sendOn(fs, wgen, goldenFrame(t, "49-granted_submit.json"))
	var refused struct{ Error ErrorBody }
	if r := fs.next(); r.Re != "m_gs1" || json.Unmarshal(r.Body, &refused) != nil || refused.Error.Code != CodeGrantRevoked {
		t.Fatalf("granted_submit under B read after its revoke: %s re %s; want %s", r.Body, r.Re, CodeGrantRevoked)
	}
	if _, reason := c.GrantLive(b.TargetID, digestB); !strings.HasPrefix(reason, CodeGrantRevoked+": ") {
		t.Fatalf("hand-over check of B while its revoke waits = %q, want %s", reason, CodeGrantRevoked)
	}
	if g, err := LoadGrants(stateDir, "example"); err != nil || g[b.Binding].Grant == nil {
		t.Fatalf("grants.json while the revoke waits: %+v, %v; want B still stored", g[b.Binding], err)
	}
	close(gate.release)
	if r := fs.next(); r.Re != "m_gi1" {
		t.Fatalf("frame %s re %s; want the grant_info reply", r.Body, r.Re)
	}
	sameBody(t, fs.next(), "m_gr2", "85-revoked_reply_v2_ended.json")
	cleared, err := LoadGrants(stateDir, "example")
	if rec := cleared[b.Binding]; err != nil || rec.Grant != nil || rec.Generation != gen || rec.IssuedFloor != "2026-10-09T14:10:00Z" {
		t.Fatalf("grants.json after ended true: %+v, %v; want no grant, generation %s and B's floor", rec, err, gen)
	}
	select {
	case n := <-notices:
		if !strings.HasSuffix(n, "ended (revoked)") {
			t.Fatalf("notice %q; want B's end", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice when the revoke ended B")
	}

	// The server did not get the answer and sends the same revoke to the
	// machine after its restart.
	fs2 := newFakeServer(t)
	c2, _ := startCarrier(t, fs2, clk, machine)
	wgen2 := fs2.waitWelcome()
	waitOnline(t, c2)
	sendOn(fs2, wgen2, namedRevokeFrame(t, digestB))
	sameBody(t, fs2.next(), "m_gr2", "86-revoked_reply_v2_not_ended.json")
	if g, err := LoadGrants(stateDir, "example"); err != nil || !reflect.DeepEqual(g[b.Binding], cleared[b.Binding]) {
		t.Fatalf("grants.json after the retry: %+v, %v; want %+v", g[b.Binding], err, cleared[b.Binding])
	}
	sendOn(fs2, wgen2, goldenFrame(t, "44-signed_grant.json"))
	if r := fs2.next(); json.Unmarshal(r.Body, &refused) != nil || refused.Error.Code != CodeGrantStale {
		t.Fatalf("signed B after its revoke and a restart: %s; want %s", r.Body, CodeGrantStale)
	}
	select {
	case n := <-notices:
		t.Fatalf("notice %q after the retry", n)
	case <-time.After(100 * time.Millisecond):
	}
}

// review finding, 2026-11-26: a revoke of one grant could end its successor.
// A machine that answers grant_revoke/2 sets grant_revoke_v2 on its hello
// envelope. The same frame and its body validate against the link schema
// as it was before the signal, and the old decode reads the same hello.
func TestHelloOffersNamedRevokeOnTheEnvelope(t *testing.T) {
	view, _ := goldenView(t)
	b := view.Bindings["pi-demo"]
	fs := newFakeServer(t)
	c, _ := startCarrier(t, fs, newClock(), func(cfg *Config) {
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = &memGrants{g: Grants{}}
	})
	fs.waitWelcome()
	waitOnline(t, c)
	fs.mu.Lock()
	raw := fs.hello
	fs.mu.Unlock()

	var outer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil || string(outer["grant_revoke_v2"]) != "true" {
		t.Fatalf("hello envelope %s; want grant_revoke_v2 true", raw)
	}

	// The link schema before the signal, beside the schemas it refers to.
	comp := jsonschema.NewCompiler()
	schemas := filepath.Join("..", "..", "..", "schemas")
	add := func(name, path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if err := comp.AddResource("https://github.com/avivsinai/agent-message-queue/schemas/"+name+".schema.json", doc); err != nil {
			t.Fatal(err)
		}
	}
	add("remote-link-v1", filepath.Join("testdata", "remote-link-v1-before-grant-revoke-v2.schema.json"))
	for _, name := range []string{"remote-command-v1", "remote-request-v1", "remote-session-v1"} {
		add(name, filepath.Join(schemas, name+".schema.json"))
	}
	frame := mustUnmarshalAnyJSON(t, raw)
	for ref, v := range map[string]any{"": frame, "#/$defs/hello": frame.(map[string]any)["body"]} {
		s, err := comp.Compile("https://github.com/avivsinai/agent-message-queue/schemas/remote-link-v1.schema.json" + ref)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(v); err != nil {
			t.Fatalf("the hello with grant_revoke_v2 against the schema before it (%q): %v", ref, err)
		}
	}

	// The old decode: the envelope ignores the member, the body is strict.
	var f Frame
	var hello helloBody
	if err := json.Unmarshal(raw, &f); err != nil || f.Schema != SchemaFrame {
		t.Fatalf("old envelope decode: %v", err)
	}
	if err := decodeStrict(f.Body, SchemaHello, &hello); err != nil {
		t.Fatalf("old hello decode: %v", err)
	}
	if hello.StoreID != "st_5d1e0c2a" || hello.AMQVersion != "test" || !reflect.DeepEqual(hello.Bindings, []Binding{b}) || len(hello.PendingLocal) != 0 {
		t.Fatalf("old decode of the hello: %+v; want the machine's store, version and binding", hello)
	}
}

func mustUnmarshalAnyJSON(t *testing.T, raw []byte) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// flakyGrants is memGrants whose next Load fails once when failNextLoad is
// set, and whose Load right after a Revoke that removed a grant fails once
// when failReadBack is set.
type flakyGrants struct {
	*memGrants
	failNextLoad, failReadBack atomic.Bool
}

func (f *flakyGrants) Load() (Grants, error) {
	if f.failNextLoad.CompareAndSwap(true, false) {
		return nil, errors.New("grants.json: input/output error")
	}
	return f.memGrants.Load()
}

func (f *flakyGrants) Revoke(binding, digest string) (bool, error) {
	changed, err := f.memGrants.Revoke(binding, digest)
	if changed && f.failReadBack.Load() {
		f.failNextLoad.Store(true)
	}
	return changed, err
}

// review finding, 2026-11-26: a revoke of one grant could end its successor.
// A named revoke whose decision fails answers storage_full and keeps the
// grant held: when the clear cannot be written, when the store cannot be
// read back after it, and when the first read fails. In that last case the
// grant expired unseen, so a clear there must not answer ended true (review
// finding: the first read's failure was ignored). Once the store works, the
// carrier's tick decides: the grant is cleared, its generation kept, and
// the hold is released.
func TestNamedRevokeStorageFailureRecovers(t *testing.T) {
	for _, fault := range []string{"write", "read back", "first read"} {
		t.Run(fault, func(t *testing.T) {
			view, key := goldenView(t)
			b := view.Bindings["pi-demo"]
			const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
			digestB := goldenGrantDigest(t)
			store := &flakyGrants{memGrants: &memGrants{g: Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key)}}}}
			fs, clk := newFakeServer(t), grantClock()
			c, _ := startCarrier(t, fs, clk, namedRevokeMachine(t, store, nil, nil))
			wgen := fs.waitWelcome()
			waitOnline(t, c)
			switch fault {
			case "write":
				store.mu.Lock()
				store.clearErr = errors.New("grants.json: no space left on device")
				store.mu.Unlock()
			case "read back":
				store.failReadBack.Store(true)
			case "first read":
				clk.advance(9 * time.Hour) // past B's not_after, never read
				store.failNextLoad.Store(true)
			}
			f := namedRevokeFrame(t, digestB)
			f.Gen = wgen
			fs.send(f)
			var refused struct{ Error ErrorBody }
			if r := fs.next(); json.Unmarshal(r.Body, &refused) != nil || refused.Error.Code != string(protocol.CodeStorageFull) {
				t.Fatalf("grant_revoke/2 with the %s failing: %s; want %s", fault, r.Body, protocol.CodeStorageFull)
			}
			if !c.namedHeld(b.Binding, digestB) {
				t.Fatalf("the hold of B was released after the %s failed", fault)
			}
			if _, reason := c.GrantLive(b.TargetID, digestB); !strings.HasPrefix(reason, CodeGrantRevoked+": ") && !strings.HasPrefix(reason, CodeGrantExpired+": ") {
				t.Fatalf("hand-over check of B after the %s failed = %q, want refused", fault, reason)
			}
			store.mu.Lock()
			store.clearErr = nil
			store.mu.Unlock()
			store.failReadBack.Store(false)
			c.Tick()
			if c.namedHeld(b.Binding, digestB) {
				t.Fatal("the tick did not settle the revoke once the store worked")
			}
			if g, err := store.Load(); err != nil || g[b.Binding].Grant != nil || g[b.Binding].Generation != gen {
				t.Fatalf("store after the recovery: %+v, %v; want no grant and generation %s", g[b.Binding], err, gen)
			}
		})
	}
}

// review finding, 2026-11-26: a revoke of one grant could end its successor.
// The connection ends while the signed grant B is still being accepted and
// a grant_revoke/2 naming B waits behind it. The revoke still decides, after
// the acceptance it follows, so B ends on disk and is refused; it never
// decides ahead of that acceptance (review finding: a revoke dropped at the
// disconnect was decided by the tick before B was stored, and B stayed).
func TestNamedRevokeAfterADisconnectFollowsItsPredecessor(t *testing.T) {
	view, _ := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	digestB := goldenGrantDigest(t)
	stateDir := namedRevokeStore(t, b.Binding, GrantRecord{Generation: gen})
	gate := newGrantGate()
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, namedRevokeMachine(t, FileGrants(stateDir, "example"), gate, nil))
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	gate.holdOn(t, fs, wgen, goldenFrame(t, "44-signed_grant.json"))
	f := namedRevokeFrame(t, digestB)
	f.Gen = wgen
	fs.send(f)
	waitFor(t, func() bool { return c.namedHeld(b.Binding, digestB) })
	fs.close(websocket.StatusGoingAway)
	fs.waitWelcome() // the machine dialed again: the first connection ended
	for range 10 {
		c.Tick()
		time.Sleep(5 * time.Millisecond)
	}
	close(gate.release)
	waitFor(t, func() bool {
		g, err := LoadGrants(stateDir, "example")
		return err == nil && g[b.Binding].Grant == nil && g[b.Binding].IssuedFloor != "" && !c.namedHeld(b.Binding, digestB)
	})
	if g, _ := LoadGrants(stateDir, "example"); g[b.Binding].Generation != gen || g[b.Binding].IssuedFloor != "2026-10-09T14:10:00Z" {
		t.Fatalf("grants.json after the revoke: %+v; want generation %s and B's floor", g[b.Binding], gen)
	}
	if _, reason := c.GrantLive(b.TargetID, digestB); reason == "" {
		t.Fatal("hand-over check of B after its revoke = live")
	}
}

// review finding, 2026-11-26: a revoke of one grant could end its successor.
// When the connection ends, a job a worker already took runs once, a named
// revoke queued behind it runs after it, and every other job not started
// never runs (review finding: the shutdown made queued revokes unordered
// retry work).
func TestDispatcherStopKeepsClaimedAndNamedJobsInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDispatcher(ctx)
	sess := &session{data: make(chan []byte, 64)}
	release := make(chan struct{})
	started := make(chan string, 8)
	var mu sync.Mutex
	var ran []string
	record := func(name string) {
		mu.Lock()
		ran = append(ran, name)
		mu.Unlock()
	}
	add := func(key, name string, keep, blocks bool) {
		j := &job{sess: sess, key: key, keep: keep, run: func() (any, error) {
			record(name + " start")
			if blocks {
				started <- name
				<-release
			}
			record(name + " end")
			return nil, nil
		}}
		if !d.enqueue(j) {
			t.Fatalf("enqueue %s refused", name)
		}
	}
	// Every worker takes a head that blocks.
	for _, key := range []string{"a", "b", "c", "d"} {
		add(key, key+"1", false, true)
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("a worker never took its job")
		}
	}
	add("a", "a2", false, false)
	add("a", "a3", true, false)
	add("e", "e1", false, false) // a head no worker took
	add("e", "e2", true, false)
	cancel()
	d.stop()
	close(release)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ran) == 12
	})
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	pos := map[string]int{}
	for i, r := range ran {
		if _, dup := pos[r]; dup {
			t.Fatalf("%s twice: %v", r, ran)
		}
		pos[r] = i
	}
	for _, name := range []string{"a1", "b1", "c1", "d1", "a3", "e2"} {
		if _, ok := pos[name+" end"]; !ok {
			t.Fatalf("%s did not run: %v", name, ran)
		}
	}
	for _, name := range []string{"a2", "e1"} {
		if _, ok := pos[name+" start"]; ok {
			t.Fatalf("%s ran after the connection ended: %v", name, ran)
		}
	}
	if pos["a3 start"] < pos["a1 end"] {
		t.Fatalf("a3 ran before a1 ended: %v", ran)
	}
	if len(ran) != 12 {
		t.Fatalf("jobs run: %v", ran)
	}
}

// review finding, 2026-11-26: a revoke of one grant could end its successor.
// Replay before the decision is written: the machine that read the revoke
// holds B only in its memory, so a restarted machine on the same state sees
// B live (the server keeps and replays the named revoke). The replayed
// revoke ends B there; the first machine's own decision then answers ended
// false. The replay after the decision is written is in
// TestNamedRevokeHoldsTheGrantUntilTheStoreDecides.
func TestNamedRevokeReplayedAfterARestartBeforeItsDecision(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	const gen = "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
	digestB := goldenGrantDigest(t)
	stateDir := namedRevokeStore(t, b.Binding, GrantRecord{Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T22:10:00Z", key)})
	gate := newGrantGate()
	fs, clk := newFakeServer(t), grantClock()
	c, _ := startCarrier(t, fs, clk, namedRevokeMachine(t, FileGrants(stateDir, "example"), gate, nil))
	wgen := fs.waitWelcome()
	waitOnline(t, c)
	gate.holdOn(t, fs, wgen, goldenFrame(t, "41-grant_info.json"))
	f := namedRevokeFrame(t, digestB)
	f.Gen = wgen
	fs.send(f)
	waitFor(t, func() bool { return c.namedHeld(b.Binding, digestB) })

	fs2 := newFakeServer(t)
	restarted, _ := startCarrier(t, fs2, clk, namedRevokeMachine(t, FileGrants(stateDir, "example"), nil, nil))
	wgen2 := fs2.waitWelcome()
	waitOnline(t, restarted)
	if _, reason := restarted.GrantLive(b.TargetID, digestB); reason != "" {
		t.Fatalf("hand-over check of B on the restarted machine = %q; want live until the replay", reason)
	}
	f.Gen = wgen2
	fs2.send(f)
	sameBody(t, fs2.next(), "m_gr2", "85-revoked_reply_v2_ended.json")
	if g, err := LoadGrants(stateDir, "example"); err != nil || g[b.Binding].Grant != nil || g[b.Binding].Generation != gen || g[b.Binding].IssuedFloor != "2026-10-09T14:10:00Z" {
		t.Fatalf("grants.json after the replay: %+v, %v; want no grant, generation %s and B's floor", g[b.Binding], err, gen)
	}

	close(gate.release)
	if r := fs.next(); r.Re != "m_gi1" {
		t.Fatalf("frame %s re %s; want the grant_info reply", r.Body, r.Re)
	}
	sameBody(t, fs.next(), "m_gr2", "86-revoked_reply_v2_not_ended.json")
}
