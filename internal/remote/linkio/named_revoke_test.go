package linkio

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	var hold atomic.Bool
	waiting, release := make(chan struct{}), make(chan struct{})
	machine := func(cfg *Config) {
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return []ConsentKey{key}, nil }
		cfg.Bindings = func() []Binding { return []Binding{b} }
		cfg.Grants = FileGrants(stateDir, "example")
		cfg.Notify = func(_, body string) { notices <- body }
		cfg.GrantInfoFor = func(Binding) (bool, string, int) {
			if hold.CompareAndSwap(true, false) {
				close(waiting)
				<-release
			}
			return true, "", protocol.MaxInputBytes
		}
		cfg.Handle = func(*protocol.Command, core.Source) (any, error) {
			t.Error("a granted task ran under a grant a revoke named")
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: "submitted"}}, nil
		}
	}
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
	hold.Store(true)
	sendOn(fs, wgen, goldenFrame(t, "41-grant_info.json"))
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("grant_info never reached the machine")
	}
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
	close(release)
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
