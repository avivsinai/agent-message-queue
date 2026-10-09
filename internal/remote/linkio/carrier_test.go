package linkio

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

const testServerID = "srv_example"

// fakeServer speaks the server side of amq.remote.link/1: it challenges,
// checks the hello signature, welcomes with the next generation, and hands
// every later frame to the test.
type fakeServer struct {
	t        *testing.T
	srv      *httptest.Server
	serverID string
	gen      atomic.Int64
	frames   chan Frame
	mu       sync.Mutex
	conn     *websocket.Conn
	ctx      context.Context
	welcomed chan int64
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{t: t, serverID: testServerID, frames: make(chan Frame, 64), welcomed: make(chan int64, 8)}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.serve))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeServer) url() string { return "ws" + strings.TrimPrefix(fs.srv.URL, "http") }

func (fs *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx := r.Context()
	nonce := b64.EncodeToString([]byte("nonce-nonce-nonce-nonce-nonce-32"))
	write(ctx, ws, Frame{Schema: SchemaFrame, ID: "c1", Body: mustJSON(challengeBody{Schema: SchemaChallenge, ServerID: fs.serverID, Nonce: nonce})})
	hf, err := readFrame(ctx, ws)
	if err != nil {
		return
	}
	var hello helloBody
	if err := decodeStrict(hf.Body, SchemaHello, &hello); err != nil || hf.Re != "c1" {
		fs.t.Errorf("hello: %v", err)
		return
	}
	pub, err := x509.ParsePKIXPublicKey(mustB64(hello.DeviceKey))
	if err != nil || !ed25519.Verify(pub.(ed25519.PublicKey), HelloMessage(fs.serverID, nonce, hello.StoreID), mustB64(hello.Signature)) {
		fs.t.Errorf("hello signature does not verify")
		return
	}
	gen := fs.gen.Add(1)
	write(ctx, ws, Frame{Schema: SchemaFrame, Re: hf.ID, Gen: gen, Body: mustJSON(welcomeBody{
		Schema: SchemaWelcome, User: "example.user", ConnectionGeneration: gen,
		Limits: Limits{FrameBytes: MaxFrameBytes, TasksInFlight: 4, ToolCallsInFlight: 8},
	})})
	fs.mu.Lock()
	fs.conn, fs.ctx = ws, ctx
	fs.mu.Unlock()
	fs.welcomed <- gen
	for {
		f, err := readFrame(ctx, ws)
		if err != nil {
			return
		}
		fs.frames <- f
	}
}

// send writes a frame on the current connection.
func (fs *fakeServer) send(f Frame) {
	fs.mu.Lock()
	ws, ctx := fs.conn, fs.ctx
	fs.mu.Unlock()
	write(ctx, ws, f)
}

func (fs *fakeServer) close(code websocket.StatusCode) {
	fs.mu.Lock()
	ws := fs.conn
	fs.mu.Unlock()
	_ = ws.Close(code, "test")
}

// next returns the next frame the endpoint sent.
func (fs *fakeServer) next() Frame {
	fs.t.Helper()
	select {
	case f := <-fs.frames:
		return f
	case <-time.After(5 * time.Second):
		fs.t.Fatal("no frame from the endpoint")
		return Frame{}
	}
}

func (fs *fakeServer) none() {
	fs.t.Helper()
	select {
	case f := <-fs.frames:
		fs.t.Fatalf("unexpected frame %s", f.Body)
	case <-time.After(100 * time.Millisecond):
	}
}

func (fs *fakeServer) waitWelcome() int64 {
	fs.t.Helper()
	select {
	case g := <-fs.welcomed:
		return g
	case <-time.After(5 * time.Second):
		fs.t.Fatal("the endpoint never completed the handshake")
		return 0
	}
}

// ack acknowledges a revision frame with the digest it carried.
func (fs *fakeServer) ack(f Frame) {
	var r revisionBody
	if err := json.Unmarshal(f.Body, &r); err != nil {
		fs.t.Fatal(err)
	}
	fs.send(Frame{Schema: SchemaFrame, Re: f.ID, Gen: f.Gen, Body: mustJSON(map[string]string{"ok": r.Digest})})
}

type clock struct{ ns atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *clock) advance(d time.Duration) { c.ns.Add(int64(d)) }
func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func mustB64(s string) []byte        { b, _ := b64.DecodeString(s); return b }
func write(ctx context.Context, ws *websocket.Conn, f Frame) {
	_ = ws.Write(ctx, websocket.MessageText, mustJSON(f))
}

// startCarrier links a carrier to fs and waits until it is online.
func startCarrier(t *testing.T, fs *fakeServer, clk *clock, tweak func(*Config)) (*Carrier, func() error) {
	t.Helper()
	key, err := MintDeviceKey(t.TempDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Name: "example", Pin: Pin{URL: fs.url(), ServerID: testServerID}, StoreID: "st_5d1e0c2a", Key: key,
		Version: "test", Now: clk.now, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		RestartDelay: time.Millisecond,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var runErr error
	done := make(chan struct{})
	go func() { runErr = c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return c, func() error {
		select {
		case <-done:
			return runErr
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
			return nil
		}
	}
}

// goldenSnapshots are the revision 4 and 6 snapshots of testdata/link, with
// the digests the contract assigns them.
func goldenSnapshots(t *testing.T) (snaps []protocol.Snapshot, digests []string) {
	t.Helper()
	for _, name := range []string{"revision_interaction", "revision_completed"} {
		paths, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "link", "frames", "*-"+name+".json"))
		if err != nil || len(paths) != 1 {
			t.Fatalf("golden frame %s: %v", name, err)
		}
		raw, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		var file struct {
			Frame struct {
				Body revisionBody `json:"body"`
			} `json:"frame"`
		}
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, file.Frame.Body.Snapshot)
		digests = append(digests, file.Frame.Body.Digest)
	}
	return snaps, digests
}

func revisionOf(t *testing.T, f Frame) revisionBody {
	t.Helper()
	var r revisionBody
	if err := decodeStrict(f.Body, SchemaRevision, &r); err != nil {
		t.Fatalf("revision frame: %v", err)
	}
	return r
}

// A revision is offered once, stays owed until the server acknowledges its
// commit, and is published (nil) only after that acknowledgement.
func TestPublishOnlyAfterAck(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	c, _ := startCarrier(t, fs, clk, nil)
	fs.waitWelcome()
	snaps, digests := goldenSnapshots(t)
	done := snaps[1]

	waitOnline(t, c)
	if err := c.Publish(done, nil); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("first Publish = %v, want pending", err)
	}
	f := fs.next()
	r := revisionOf(t, f)
	if r.Digest != digests[1] || r.Revision != 6 {
		t.Fatalf("offered digest %s rev %d, want the golden %s rev 6", r.Digest, r.Revision, digests[1])
	}
	if err := c.Publish(done, nil); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("second Publish = %v, want pending", err)
	}
	fs.none() // in flight: not offered again
	fs.ack(f)
	waitPublished(t, c, done)
}

// A lost acknowledgement leaves the offer to expire; the next sweep after
// the offer's lifetime sends the revision again.
func TestLostAckIsResent(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	c, _ := startCarrier(t, fs, clk, nil)
	fs.waitWelcome()
	waitOnline(t, c)
	snaps, _ := goldenSnapshots(t)
	_ = c.Publish(snaps[1], nil)
	first := fs.next()
	clk.advance(DefaultOfferTTL + time.Second)
	if err := c.Publish(snaps[1], nil); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("Publish after expiry = %v, want pending", err)
	}
	second := fs.next()
	if second.ID == first.ID || revisionOf(t, second).Revision != 6 {
		t.Fatal("the expired offer was not sent again as a new frame")
	}
	fs.ack(second)
	waitPublished(t, c, snaps[1])
}

// A reconnect starts a new generation with an empty cache: every revision
// still owed is offered again, and an answer from the old generation counts
// for nothing.
func TestReconnectResendsOwed(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	c, _ := startCarrier(t, fs, clk, nil)
	g1 := fs.waitWelcome()
	waitOnline(t, c)
	snaps, _ := goldenSnapshots(t)
	_ = c.Publish(snaps[0], nil)
	_ = c.Publish(snaps[1], nil)
	old := fs.next()
	fs.next()
	fs.close(websocket.StatusCode(1012))
	g2 := fs.waitWelcome()
	if g2 <= g1 {
		t.Fatalf("generation %d after %d", g2, g1)
	}
	waitGen(t, c, g2)
	fs.send(Frame{Schema: SchemaFrame, Re: old.ID, Gen: g1, Body: mustJSON(map[string]string{"ok": revisionOf(t, old).Digest})})
	for _, s := range snaps {
		if err := c.Publish(s, nil); !errors.Is(err, ErrPublishPending) {
			t.Fatalf("Publish rev %d after reconnect = %v, want pending", s.Revision, err)
		}
	}
	a, b := fs.next(), fs.next()
	if a.Gen != g2 || b.Gen != g2 {
		t.Fatal("owed revisions were not offered on the new generation")
	}
	fs.ack(a)
	fs.ack(b)
	waitPublished(t, c, snaps[1])
}

// An acknowledgement for an older revision offered on the same generation
// counts for that revision; the newer one stays owed until its own.
func TestAckForOlderOfferCounts(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	c, _ := startCarrier(t, fs, clk, nil)
	fs.waitWelcome()
	waitOnline(t, c)
	snaps, _ := goldenSnapshots(t)
	_ = c.Publish(snaps[0], nil)
	r4 := fs.next()
	_ = c.Publish(snaps[1], nil)
	r6 := fs.next()
	fs.ack(r4)
	waitPublished(t, c, snaps[0])
	if err := c.Publish(snaps[1], nil); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("rev 6 after rev 4's ack = %v, want pending", err)
	}
	fs.ack(r6)
	waitPublished(t, c, snaps[1])
}

// A server that names another server id is refused before hello.
func TestHandshakeRefusesAnotherServer(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	fs.serverID = "srv_other"
	c, _ := startCarrier(t, fs, clk, nil)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.Status().Error, "pinned") {
		if time.Now().After(deadline) {
			t.Fatalf("status %+v: want a refusal naming the pinned server", c.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Close 4010 is a permanent revoke: Run stops and the sink is retired.
func TestRevokeRetires(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	var retired atomic.Bool
	_, wait := startCarrier(t, fs, clk, func(cfg *Config) { cfg.Revoked = func() { retired.Store(true) } })
	fs.waitWelcome()
	fs.close(CloseRevoked)
	if err := wait(); !errors.Is(err, ErrRevoked) || !retired.Load() {
		t.Fatalf("Run = %v, retired %v; want ErrRevoked and a retirement", err, retired.Load())
	}
}

// A request the link cannot serve yet is refused on the control lane.
func TestUnsupportedRequestIsRefused(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	c, _ := startCarrier(t, fs, clk, nil)
	gen := fs.waitWelcome()
	waitOnline(t, c)
	fs.send(Frame{Schema: SchemaFrame, ID: "m_s1", Gen: gen, Body: mustJSON(map[string]string{"schema": protocol.SchemaCommand, "op": "session.list"})})
	f := fs.next()
	var r errorReply
	if err := decodeStrict(f.Body, "", &r); err != nil || f.Re != "m_s1" || r.Error.Code != string(protocol.CodeUnsupported) {
		t.Fatalf("reply %s: want unsupported for m_s1", f.Body)
	}
}

func waitOnline(t *testing.T, c *Carrier) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.Status().State != "online" {
		if time.Now().After(deadline) {
			t.Fatal("never online")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitGen(t *testing.T, c *Carrier, gen int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s := c.Status(); s.State != "online" || s.Gen != gen; s = c.Status() {
		if time.Now().After(deadline) {
			t.Fatalf("never online on generation %d", gen)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitPublished polls Publish the way core's sweep does until it returns nil.
func waitPublished(t *testing.T, c *Carrier, s protocol.Snapshot) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := c.Publish(s, nil)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rev %d never published: %v", s.Revision, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// The fingerprint this machine computes equals the one the contract pins for
// the golden consent key, so a person compares like with like.
func TestFingerprintMatchesContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "link", "consent", "assertion.json"))
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		ConsentKey
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	got, err := Fingerprint(a.ConsentKey)
	if err != nil || got != a.Fingerprint || !SameFingerprint(strings.ToLower(got), a.Fingerprint) {
		t.Fatalf("Fingerprint = %s, %v; the contract says %s", got, err, a.Fingerprint)
	}
}

// busy keeps the work: the revision is not offered again before the server's
// retry_after_ms, and is offered again after it.
func TestBusyRevisionWaitsForRetryAfter(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	c, _ := startCarrier(t, fs, clk, nil)
	fs.waitWelcome()
	waitOnline(t, c)
	snaps, _ := goldenSnapshots(t)
	_ = c.Publish(snaps[1], nil)
	f := fs.next()
	fs.send(Frame{Schema: SchemaFrame, Re: f.ID, Gen: f.Gen, Body: mustJSON(errorReply{Error: ErrorBody{Code: "busy", RetryAfterMS: 5000}})})
	waitFor(t, func() bool { return c.offerExpiry(snaps[1]).Sub(clk.now()) == 5*time.Second })
	clk.advance(4 * time.Second)
	if err := c.Publish(snaps[1], nil); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("Publish during retry_after = %v, want pending", err)
	}
	fs.none()
	clk.advance(2 * time.Second)
	_ = c.Publish(snaps[1], nil)
	if r := revisionOf(t, fs.next()); r.Revision != 6 {
		t.Fatalf("re-offered revision %d, want 6", r.Revision)
	}
}

// A conflict (the server holds this revision with another digest) is
// terminal: the revision stays owed and is never sent again.
func TestConflictIsNeverResent(t *testing.T) {
	fs, clk := newFakeServer(t), newClock()
	c, _ := startCarrier(t, fs, clk, nil)
	fs.waitWelcome()
	waitOnline(t, c)
	snaps, _ := goldenSnapshots(t)
	_ = c.Publish(snaps[1], nil)
	f := fs.next()
	fs.send(Frame{Schema: SchemaFrame, Re: f.ID, Gen: f.Gen, Body: mustJSON(errorReply{Error: ErrorBody{Code: "conflict"}})})
	waitFor(t, func() bool { return c.Status().Conflicts == 1 })
	clk.advance(DefaultOfferTTL + time.Second)
	if err := c.Publish(snaps[1], nil); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("Publish after a conflict = %v, want pending (owed, not sent)", err)
	}
	fs.none()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
	}
}
