package amit

// Tests for the amit-remote contract v1 adapter. The contract
// (packages/amit/extensions/amit-remote/CONTRACT.md, amit master 1e21930a)
// governs every assertion here; section references are in the test names
// and comments. Real-file tests cover the §5 recovery table verbatim —
// including rotation tolerance and a partial last line — against an actual
// extension directory layout, not a fake source.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// testKey builds a record key and its contract ref. The request id must be
// a protocol-valid UUID (protocol.EncodeRef/DecodeRef require the canonical
// shape), so the label is folded into the UUID's deterministic namespace.
func testKey(id string) requests.Key {
	return requests.Key{
		CreatorHost: "host1",
		TargetID:    "amit",
		RequestID:   fmt.Sprintf("00000000-0000-4000-8000-%012x", fnv32a(id)%(1<<48)),
	}
}

// fnv32a is a tiny deterministic hash for stable test request ids.
func fnv32a(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// fixedNow pins "now" so heartbeat windows are deterministic.
var fixedNow = time.Unix(1700000000, 0)

// newTestAttachment builds an attachment over a real temp extension dir
// with a LIVE liveness stamp (the common case; tests flip it explicitly).
func newTestAttachment(t *testing.T) (*Attachment, string) {
	t.Helper()
	dir := newExtDir(t)
	return mustAttach(t, dir), dir
}

// newExtDir creates the contract's directory layout.
func newExtDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "agent1", "extensions", "amit-remote")
	for _, sub := range []string{"requests", "receipts", "events"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	stampLiveness(t, dir, fixedNow)
	return dir
}

// stampLiveness writes a fresh bridge.liveness at the given time.
func stampLiveness(t *testing.T, dir string, at time.Time) {
	t.Helper()
	rec := fmt.Sprintf(`{"protocol":%q,"live":true,"at":%q,"pid":%d,"surface":"app"}`,
		ProtocolV1, at.UTC().Format(time.RFC3339Nano), os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "bridge.liveness"), []byte(rec), 0o600); err != nil {
		t.Fatalf("write liveness: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, "bridge.liveness"), at, at); err != nil {
		t.Fatalf("chtimes liveness: %v", err)
	}
}

// stampLivenessWithProtocol writes a bridge.liveness with a custom protocol
// string (the §9 test seam).
func stampLivenessWithProtocol(t *testing.T, dir string, at time.Time, proto string) {
	t.Helper()
	rec := fmt.Sprintf(`{"protocol":%q,"live":true,"at":%q,"pid":%d,"surface":"app"}`,
		proto, at.UTC().Format(time.RFC3339Nano), os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "bridge.liveness"), []byte(rec), 0o600); err != nil {
		t.Fatalf("write liveness: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, "bridge.liveness"), at, at); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// writeReceipt / writeEvents / writeRequest lay down extension-side files.
func writeReceipt(t *testing.T, dir, ref, gen string, at time.Time) {
	t.Helper()
	rc := fmt.Sprintf(`{"protocol":%q,"ref":%q,"session_generation":%q,"delivered_at":%q,"pid":%d}`,
		ProtocolV1, ref, gen, at.UTC().Format(time.RFC3339Nano), os.Getpid())
	name := filepath.Join(dir, "receipts", refSanitize(ref)+".json")
	if err := os.WriteFile(name, []byte(rc), 0o600); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	if err := os.Chtimes(name, at, at); err != nil {
		t.Fatalf("chtimes receipt: %v", err)
	}
}

func appendEvents(t *testing.T, dir, ref string, lines ...string) {
	t.Helper()
	name := filepath.Join(dir, "events", refSanitize(ref)+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer func() { _ = f.Close() }()
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
}

func mustAttach(t *testing.T, dir string) *Attachment {
	t.Helper()
	a, err := New("amit", "agent1", bridgeDir{dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.now = func() time.Time { return fixedNow }
	a.submitWait = 50 * time.Millisecond
	return a
}

// seedRequest pre-publishes a request file (what Submit writes) so
// recovery tests can arrange receipts/events around an existing request.
func seedRequest(t *testing.T, dir, ref string, epochHint string) {
	t.Helper()
	payload := fmt.Sprintf(`{"ref":%q,"text":"hello","deliver_as":"followUp","not_after":"2036-01-01T00:00:00Z","epoch_hint":%q,"created_at":"2035-01-01T00:00:00Z"}`,
		ref, epochHint)
	name := filepath.Join(dir, "requests", refSanitize(ref)+".json")
	if err := os.WriteFile(name, []byte(payload), 0o600); err != nil {
		t.Fatalf("seed request: %v", err)
	}
}

func submitReq(key requests.Key, text string) core.BoundRequest {
	return core.BoundRequest{Key: key, Epoch: SentinelUnpinned, Input: protocol.SubmitInput{Text: text}, NotAfter: "2036-01-01T00:00:00Z"}
}

// --- Inspect & epoch (§4/A4, §7) -----------------------------------------

// TestInspectUnpinnedSentinel pins A4: before the first receipt the epoch
// is the non-empty sentinel `unpinned`, never "" (an empty epoch would
// defeat stale-epoch protection because "" == "" always passes).
func TestInspectUnpinnedSentinel(t *testing.T) {
	a, _ := newTestAttachment(t)
	s := a.Inspect()
	if s.Epoch != SentinelUnpinned || s.Epoch == "" {
		t.Fatalf("epoch = %q, want sentinel %q", s.Epoch, SentinelUnpinned)
	}
	if !protocol.ValidEpoch(s.Epoch) {
		t.Fatalf("epoch %q is not protocol-valid", s.Epoch)
	}
}

// TestFirstReceiptPinsEpoch pins §4: the first receipt's session_generation
// becomes the published epoch; only receipts pin (never liveness, never the
// session id).
func TestFirstReceiptPinsEpoch(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("r1")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-7", fixedNow)
	// Submit against an existing receipt: recovery/refresh binds and pins.
	adm, err := a.Submit(core.BoundRequest{Key: key, Epoch: SentinelUnpinned, Input: protocol.SubmitInput{Text: "hello"}, NotAfter: "2036-01-01T00:00:00Z"})
	if err != nil || !adm.Admitted {
		t.Fatalf("Submit = %+v, %v; want receipt-gated admission", adm, err)
	}
	s := a.Inspect()
	if s.Epoch != "gen-7" {
		t.Fatalf("epoch after first receipt = %q, want gen-7", s.Epoch)
	}
}

// TestLaterSubmitCarriesEpochHint pins §1+§4: a submit after pinning
// publishes the pinned generation as epoch_hint (empty only while
// unpinned), and the request JSON carries deliver_as followUp + not_after.
func TestLaterSubmitCarriesEpochHint(t *testing.T) {
	a, dir := newTestAttachment(t)
	// Pin via an existing receipt for another ref.
	other := clientRef(testKey("seed"))
	seedRequest(t, dir, other, "")
	writeReceipt(t, dir, other, "gen-9", fixedNow)
	if _, err := a.Submit(submitReq(testKey("seed"), "hello")); err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	if a.Inspect().Epoch != "gen-9" {
		t.Fatalf("want pinned gen-9, got %q", a.Inspect().Epoch)
	}
	// New submit: receipt arrives mid-poll via a watcher goroutine. The
	// request file is written by Submit BEFORE it polls, so assert the hint
	// from the file the first poll observes.
	key := testKey("r2")
	ref := clientRef(key)
	if _, ok := a.runs[key]; ok {
		t.Fatal("fresh key must not be pre-bound")
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		writeReceipt(t, dir, ref, "gen-9", fixedNow)
	}()
	if _, err := a.Submit(core.BoundRequest{Key: key, Epoch: "gen-9", Input: protocol.SubmitInput{Text: "q"}, NotAfter: "2036-01-01T00:00:00Z"}); err != nil {
		t.Fatalf("submit with pinned epoch: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests", refSanitize(ref)+".json"))
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	body := string(data)
	for _, want := range []string{`"deliver_as":"followUp"`, `"epoch_hint":"gen-9"`, `"not_after":"2036-01-01T00:00:00Z"`, `"ref":"` + ref + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("request JSON missing %s:\n%s", want, body)
		}
	}
}

// TestInspectCapabilities pins §7: submit true with evidence `submitted`,
// steer false, cancel/approve/question false, terminal unavailable, and
// offline when the heartbeat is stale.
func TestInspectCapabilities(t *testing.T) {
	a, dir := newTestAttachment(t)
	s := a.Inspect()
	c := s.Capabilities
	if !c.Inspect || !c.Submit || c.Steer || c.CancelRequest || c.ApproveTool || c.AnswerQuestion {
		t.Fatalf("capabilities = %+v, want submit-only, steer false", c)
	}
	if c.Terminal != "unavailable" {
		t.Fatalf("terminal = %q, want unavailable", c.Terminal)
	}
	if s.Evidence == nil || s.Evidence.Submit != protocol.EvidenceSubmitted {
		t.Fatalf("evidence = %+v, want submit=submitted", s.Evidence)
	}
	if s.Harness != "amit" || s.Attachment != "live" {
		t.Fatalf("session = %+v, want live amit projection", s)
	}
	// Stale heartbeat → offline.
	stampLiveness(t, dir, fixedNow.Add(-time.Minute))
	if s := a.Inspect(); s.Attachment != "offline" || s.Status != "offline" {
		t.Fatalf("stale liveness: attachment=%s status=%s, want offline", s.Attachment, s.Status)
	}
}

// --- Submit: receipt-gated admission, liveness, duplicates (§1, §2) -------

// TestSubmitAdmittedOnlyWithReceipt pins 9b: Admitted:true is returned only
// when the receipt lands; a live bridge without one leaves the submit
// UNCERTAIN (non-nil error, correlation bound), never admitted.
func TestSubmitAdmittedOnlyWithReceipt(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("u1")
	ref := clientRef(key)
	// Deterministic fixture (not live extension proof): the receipt is
	// written by the publish hook — after the REAL publishRequest succeeds,
	// before Submit's first poll step — instead of a sleeping producer
	// racing the poll window. Scoped to u1's ref only: u2's submit must
	// still find no receipt and exercise the uncertain path.
	a.dir.publish = func(req deliverRequest) error {
		if err := (bridgeDir{dir: dir}).publishRequest(req); err != nil {
			return err
		}
		if req.Ref == ref {
			writeReceipt(t, dir, ref, "gen-1", fixedNow)
		}
		return nil
	}
	adm, err := a.Submit(submitReq(key, "hello"))
	if err != nil || !adm.Admitted || adm.RunID == "" {
		t.Fatalf("Submit with receipt = %+v, %v; want admitted with run id", adm, err)
	}
	// No receipt, live bridge: uncertain. The endpoint sends the pinned
	// epoch it read from Inspect.
	pinned := core.BoundRequest{Key: testKey("u2"), Epoch: a.Inspect().Epoch, Input: protocol.SubmitInput{Text: "hello"}, NotAfter: "2036-01-01T00:00:00Z"}
	adm2, err2 := a.Submit(pinned)
	if err2 == nil || adm2.Admitted || adm2.RunID == "" {
		t.Fatalf("Submit without receipt = %+v, %v; want uncertain with correlation bound", adm2, err2)
	}
	// A refusal code would commit a terminal rejected record — forbidden on
	// the ambiguous path.
	if adm2.Code != "" {
		t.Fatalf("uncertain submit carries code %q, want none", adm2.Code)
	}
}

// TestSubmitDeadBridgeFailsPreSideEffect pins §2: no receipt + dead
// liveness → FAILED pre-side-effect; a fresh submit writes NO request file
// (nobody is listening), and the refusal is positive.
func TestSubmitDeadBridgeFailsPreSideEffect(t *testing.T) {
	a, dir := newTestAttachment(t)
	stampLiveness(t, dir, fixedNow.Add(-time.Hour)) // stale
	adm, err := a.Submit(submitReq(testKey("d1"), "hello"))
	// Positive refusal rides Admission.Code (nil error): the endpoint's
	// admissionCause maps a non-nil native error to attachment_lost/
	// UNCERTAIN, never to a positive refusal — so a definitive code must
	// never be delivered as an error.
	if err != nil || adm.Admitted || adm.Code != protocol.CodeAttachmentLost {
		t.Fatalf("Submit = %+v, %v; want positive attachment_lost refusal", adm, err)
	}
	entries, rerr := os.ReadDir(filepath.Join(dir, "requests"))
	if rerr != nil || len(entries) != 0 {
		t.Fatalf("requests dir = %v (%d entries), want empty (pre-side-effect)", rerr, len(entries))
	}
}

// TestSubmitDuplicateRefNeverRewrites pins §1: the request file is
// create-new; a duplicate publish refuses positively and the file content
// is untouched (never double-fired).
func TestSubmitDuplicateRefNeverRewrites(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("dup")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "") // existing delivery from a previous process
	before, _ := os.ReadFile(filepath.Join(dir, "requests", refSanitize(ref)+".json"))
	err := a.dir.publishRequest(deliverRequest{Ref: ref, Text: "SECOND"})
	if !errors.Is(err, ErrAlreadyDelivered) {
		t.Fatalf("publish err = %v, want ErrAlreadyDelivered", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "requests", refSanitize(ref)+".json"))
	if string(before) != string(after) {
		t.Fatal("duplicate publish overwrote the request file")
	}
}

// TestPublishRequestAtomicShape pins §1's atomic write: the published file
// contains the full payload, no temp files leak, and the request dir still
// holds only ref-named files.
func TestPublishRequestAtomicShape(t *testing.T) {
	_, dir := newTestAttachment(t)
	ref := clientRef(testKey("atomic"))
	bd := bridgeDir{dir: dir}
	err := bd.publishRequest(deliverRequest{
		Ref: ref, Text: "body", DeliverAs: "followUp", NotAfter: "2036-01-01T00:00:00Z", CreatedAt: protocol.FormatTime(fixedNow),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests", refSanitize(ref)+".json"))
	if err != nil || !strings.Contains(string(data), `"text":"body"`) {
		t.Fatalf("published request = %q, %v", data, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "requests"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".publish-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// TestSubmitSteerRefused pins §7: deliver=steer is refused pre-side-effect
// (unsupported), before any file write.
func TestSubmitSteerRefused(t *testing.T) {
	a, dir := newTestAttachment(t)
	req := submitReq(testKey("s1"), "redirect")
	req.Input.Deliver = protocol.DeliverSteer
	adm, err := a.Submit(req)
	if err != nil || adm.Admitted || adm.Code != protocol.CodeUnsupported {
		t.Fatalf("Submit = %+v, %v; want positive unsupported refusal", adm, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "requests"))
	if len(entries) != 0 {
		t.Fatalf("requests dir = %d entries, want 0", len(entries))
	}
}

// --- Lookup / evidence (§5) ------------------------------------------------

// TestLookupUnretainedKeyUnknown pins the Amit evidence rule: a key the
// adapter retains nothing about is EvidenceUnknown, never EvidenceNone.
func TestLookupUnretainedKeyUnknown(t *testing.T) {
	a, _ := newTestAttachment(t)
	ev, err := a.Lookup(testKey("never"), SentinelUnpinned)
	if err != nil || ev.Class != core.EvidenceUnknown {
		t.Fatalf("evidence = %+v, %v; want unknown", ev, err)
	}
}

// TestLookupReceiptNoEventsConfirmedRunning pins §5 row 3: receipt present,
// no events (rotated/absent log) → confirmed-running, never uncertain.
func TestLookupReceiptNoEventsConfirmedRunning(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("rc")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	ev, err := a.Lookup(key, "gen-1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ev.Class != core.EvidenceConfirmed || !ev.Admitted || ev.State != protocol.StateRunning {
		t.Fatalf("evidence = %+v, want confirmed running", ev)
	}
}

// TestLookupTerminalEvents pins §5 rows 1: receipt + completed/failed/
// cancelled events resolve the terminal states with results.
func TestLookupTerminalEvents(t *testing.T) {
	a, dir := newTestAttachment(t)
	cases := []struct {
		event string
		line  string
		state protocol.State
	}{
		{"completed", fmt.Sprintf(`{"protocol":%q,"ref":"REF","event":"completed","text":"the answer"}`, ProtocolV1), protocol.StateCompleted},
		{"failed", fmt.Sprintf(`{"protocol":%q,"ref":"REF","event":"failed","error":"boom"}`, ProtocolV1), protocol.StateFailed},
		{"cancelled", fmt.Sprintf(`{"protocol":%q,"ref":"REF","event":"cancelled"}`, ProtocolV1), protocol.StateCancelled},
	}
	for i, tc := range cases {
		key := testKey(fmt.Sprintf("t%d", i))
		ref := clientRef(key)
		seedRequest(t, dir, ref, "")
		writeReceipt(t, dir, ref, "gen-1", fixedNow)
		appendEvents(t, dir, ref, strings.ReplaceAll(tc.line, "REF", ref))
		ev, err := a.Lookup(key, "gen-1")
		if err != nil {
			t.Fatalf("%s Lookup: %v", tc.event, err)
		}
		if ev.Class != core.EvidenceHistoryTerminated || ev.State != tc.state {
			t.Fatalf("%s evidence = %+v, want history-terminated %s", tc.event, ev, tc.state)
		}
		switch tc.event {
		case "completed":
			if ev.Result == nil || ev.Result.Text != "the answer" {
				t.Fatalf("completed result = %+v, want answer text", ev.Result)
			}
		case "failed":
			if ev.Result == nil || ev.Result.Error != "boom" {
				t.Fatalf("failed result = %+v, want error", ev.Result)
			}
		}
	}
}

// TestLookupNoReceiptUncertain pins §5 row 4: no receipt → uncertain, the
// record keeps correlating (the send primitive cannot prove non-admission).
func TestLookupNoReceiptUncertain(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("nc")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	if _, err := a.Submit(submitReq(key, "hello")); err != nil {
		// Submit may time out uncertain — that is the expected shape.
	} else {
		t.Fatal("submit without receipt must be uncertain")
	}
	ev, err := a.Lookup(key, SentinelUnpinned)
	if err != nil || ev.Class != core.EvidenceUnknown {
		t.Fatalf("evidence = %+v, %v; want unknown (uncertain)", ev, err)
	}
}

// TestAcknowledgeReleasesResult pins the ack contract with the stable
// digest; a wrong digest releases nothing.
func TestAcknowledgeReleasesResult(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("ack")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	appendEvents(t, dir, ref, fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"completed","text":"answer"}`, ProtocolV1, ref))
	ev, _ := a.Lookup(key, "gen-1")
	if ev.Result == nil {
		t.Fatal("want retained result before ack")
	}
	a.AcknowledgeResult(key, "gen-1", protocol.EvidenceDigest(&protocol.Result{Text: "other"}))
	if ev2, _ := a.Lookup(key, "gen-1"); ev2.Class == core.EvidenceNone {
		t.Fatal("wrong digest must not release the result")
	}
	a.AcknowledgeResult(key, "gen-1", protocol.EvidenceDigestStable(ev.Result))
	if ev3, _ := a.Lookup(key, "gen-1"); ev3.Class != core.EvidenceNone {
		t.Fatalf("evidence after ack = %q, want none", ev3.Class)
	}
}

// --- A3: fire-time expiry over proven admission ----------------------------

// TestExpiredRefusalMapsTyped pins §6/A3: receipt present + refused(expired)
// → Lookup reports proven admission with RefusalCode expired — the endpoint
// maps it to rejected+expired, never a silent dispatch.
func TestExpiredRefusalMapsTyped(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("exp")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-1", fixedNow) // admission proven, receipt STAYS
	appendEvents(t, dir, ref, fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"refused","reason":"expired"}`, ProtocolV1, ref))
	ev, err := a.Lookup(key, "gen-1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ev.Admitted || ev.State != protocol.StateRejected || ev.RefusalCode != protocol.CodeExpired {
		t.Fatalf("evidence = %+v, want admitted rejected with refusal code expired", ev)
	}
}

// TestGenerationRefusalDropsToSentinel pins §4: a refused(generation) event
// proves the pinned epoch stale; the adapter drops back to the `unpinned`
// sentinel so the next receipt re-pins the live generation.
func TestGenerationRefusalDropsToSentinel(t *testing.T) {
	a, dir := newTestAttachment(t)
	seed := clientRef(testKey("seed"))
	seedRequest(t, dir, seed, "")
	writeReceipt(t, dir, seed, "gen-1", fixedNow)
	if _, err := a.Submit(submitReq(testKey("seed"), "hello")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if a.Inspect().Epoch != "gen-1" {
		t.Fatalf("want gen-1 pinned, got %q", a.Inspect().Epoch)
	}
	key := testKey("g1")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "gen-1")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	appendEvents(t, dir, ref, fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"refused","reason":"generation"}`, ProtocolV1, ref))
	if _, err := a.Lookup(key, "gen-1"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if s := a.Inspect(); s.Epoch != SentinelUnpinned {
		t.Fatalf("epoch after generation refusal = %q, want sentinel %q", s.Epoch, SentinelUnpinned)
	}
}

// --- §5 restart recovery against real files --------------------------------

// TestRecoveryTableVerbatim drives the §5 recovery table against a real
// extension directory: a fresh attachment over the same files answers from
// history and never redispatches.
func TestRecoveryTableVerbatim(t *testing.T) {
	dir := newExtDir(t)
	ref := clientRef(testKey("rec"))

	// Row 1: receipt + terminal event → terminal state, result from event.
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	appendEvents(t, dir, ref,
		fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"started"}`, ProtocolV1, ref),
		fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"completed","text":"recovered answer"}`, ProtocolV1, ref))
	a := mustAttach(t, dir)
	ev, err := a.Lookup(testKey("rec"), "gen-1")
	if err != nil || ev.Class != core.EvidenceHistoryTerminated || ev.State != protocol.StateCompleted {
		t.Fatalf("row1 = %+v, %v; want history-terminated completed", ev, err)
	}
	if ev.Result == nil || ev.Result.Text != "recovered answer" {
		t.Fatalf("row1 result = %+v, want recovered text", ev.Result)
	}

	// Row 2: receipt + only started → running.
	ref2 := clientRef(testKey("rec2"))
	seedRequest(t, dir, ref2, "")
	writeReceipt(t, dir, ref2, "gen-1", fixedNow)
	appendEvents(t, dir, ref2, fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"started"}`, ProtocolV1, ref2))
	a = mustAttach(t, dir)
	if ev, err := a.Lookup(testKey("rec2"), "gen-1"); err != nil || ev.Class != core.EvidenceConfirmed || ev.State != protocol.StateRunning {
		t.Fatalf("row2 = %+v, %v; want confirmed running", ev, err)
	}

	// Row 3: receipt, no events (rotated/absent log) → confirmed-running.
	ref3 := clientRef(testKey("rec3"))
	seedRequest(t, dir, ref3, "")
	writeReceipt(t, dir, ref3, "gen-1", fixedNow)
	a = mustAttach(t, dir)
	if ev, err := a.Lookup(testKey("rec3"), "gen-1"); err != nil || ev.Class != core.EvidenceConfirmed || ev.State != protocol.StateRunning {
		t.Fatalf("row3 = %+v, %v; want confirmed running (never uncertain)", ev, err)
	}

	// Row 4: no receipt → uncertain (not bound at attach; Lookup unknown).
	ref4 := clientRef(testKey("rec4"))
	seedRequest(t, dir, ref4, "")
	a = mustAttach(t, dir)
	if ev, err := a.Lookup(testKey("rec4"), SentinelUnpinned); err != nil || ev.Class != core.EvidenceUnknown {
		t.Fatalf("row4 = %+v, %v; want unknown/uncertain", ev, err)
	}

	// §5: an existing request file is never redispatched — recovery binds
	// from receipts/events only; the request files above were never
	// rewritten (mtime unchanged is over-pinning; content identity is the
	// assertion).
	for _, r := range []string{ref, ref2, ref3, ref4} {
		data, rerr := os.ReadFile(filepath.Join(dir, "requests", refSanitize(r)+".json"))
		if rerr != nil || !strings.Contains(string(data), `"text":"hello"`) {
			t.Fatalf("request %s disturbed by recovery: %q, %v", r, data, rerr)
		}
	}
}

// TestRecoveryPinsNewestGeneration pins §4 across restart: with receipts
// from two generations the recovered epoch is the NEWEST receipt's
// generation.
func TestRecoveryPinsNewestGeneration(t *testing.T) {
	dir := newExtDir(t)
	oldRef := clientRef(testKey("old"))
	newRef := clientRef(testKey("new"))
	seedRequest(t, dir, oldRef, "")
	seedRequest(t, dir, newRef, "")
	writeReceipt(t, dir, oldRef, "gen-1", fixedNow)
	writeReceipt(t, dir, newRef, "gen-2", fixedNow.Add(time.Second))
	a := mustAttach(t, dir)
	if s := a.Inspect(); s.Epoch != "gen-2" {
		t.Fatalf("recovered epoch = %q, want gen-2 (newest receipt)", s.Epoch)
	}
}

// TestRecoveryToleratesRotatedLogAndPartialLine pins A2 + §3: a truncated
// trailing line (crash mid-append) is skipped; the fsynced terminal line
// still resolves the record.
func TestRecoveryToleratesRotatedLogAndPartialLine(t *testing.T) {
	dir := newExtDir(t)
	ref := clientRef(testKey("rot"))
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	// A complete line, then a partial line (no trailing newline, invalid).
	content := fmt.Sprintf("{\"protocol\":%q,\"ref\":%q,\"event\":\"started\"}\n{\"protocol\":%q,\"ref\":%q,\"event\":\"compl",
		ProtocolV1, ref, ProtocolV1, ref)
	if err := os.WriteFile(filepath.Join(dir, "events", refSanitize(ref)+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write events: %v", err)
	}
	a := mustAttach(t, dir)
	ev, err := a.Lookup(testKey("rot"), "gen-1")
	if err != nil || ev.Class != core.EvidenceConfirmed || ev.State != protocol.StateRunning {
		t.Fatalf("evidence = %+v, %v; want confirmed running (partial line skipped)", ev, err)
	}
}

// --- §9 protocol refusal ----------------------------------------------------

// TestForeignProtocolReceiptRefused pins §9: a receipt carrying an unknown
// protocol string is refused, never guessed into evidence (the run stays
// uncertain).
func TestForeignProtocolReceiptRefused(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("fp")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	rc := fmt.Sprintf(`{"protocol":"amit:amq-remote:v9","ref":%q,"session_generation":"gen-x","delivered_at":"","pid":1}`, ref)
	if err := os.WriteFile(filepath.Join(dir, "receipts", refSanitize(ref)+".json"), []byte(rc), 0o600); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	ev, err := a.Lookup(key, SentinelUnpinned)
	if err == nil || ev.Class == core.EvidenceConfirmed {
		t.Fatalf("evidence = %+v, %v; want refusal (unknown protocol)", ev, err)
	}
}

// TestForeignProtocolLivenessNotLive pins §9 on the write path: a
// bridge.liveness carrying an unknown protocol string is NOT a live bridge —
// the pre-gate refuses with attachment_lost and NO request file is written
// into the foreign seam.
func TestForeignProtocolLivenessNotLive(t *testing.T) {
	a, dir := newTestAttachment(t)
	stampLivenessWithProtocol(t, dir, fixedNow, "amit:amq-remote:v9")
	adm, err := a.Submit(submitReq(testKey("fp9"), "hello"))
	if err != nil || adm.Admitted || adm.Code != protocol.CodeAttachmentLost {
		t.Fatalf("Submit = %+v, %v; want positive attachment_lost refusal (foreign protocol)", adm, err)
	}
	entries, rerr := os.ReadDir(filepath.Join(dir, "requests"))
	if rerr != nil || len(entries) != 0 {
		t.Fatalf("requests dir = %v (%d entries), want empty (no write into a foreign seam)", rerr, len(entries))
	}
}

// TestForeignProtocolEventsRefused pins §9/P2: with a v1 receipt and a
// v2-only event stream the stream is REFUSED (an error), never read as "no
// events" — recovery row 3 must not map a hidden terminal to
// confirmed-running.
func TestForeignProtocolEventsRefused(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("fpe")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	appendEvents(t, dir, ref, fmt.Sprintf(`{"protocol":"amit:amq-remote:v2","event":"completed","ref":%q,"text":"done"}`, ref))
	ev, err := a.Lookup(key, "gen-1")
	if err == nil {
		t.Fatalf("evidence = %+v, nil error; want refusal (foreign-protocol event stream)", ev)
	}
	if ev.Class == core.EvidenceConfirmed || ev.Class == core.EvidenceHistoryTerminated {
		t.Fatalf("evidence = %+v; foreign-protocol stream must not become evidence", ev)
	}
}

// --- Cancel & misc -----------------------------------------------------------

// TestCancelUnsupported pins §7: no native cancel seam; terminal runs are
// noop_already_terminal.
func TestCancelUnsupported(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("c1")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	ev, err := a.CancelExact(key, "gen-1")
	if err != nil || ev.Disposition != protocol.CancelUnsupported {
		t.Fatalf("cancel = %+v, %v; want unsupported", ev, err)
	}
	appendEvents(t, dir, ref, fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"cancelled"}`, ProtocolV1, ref))
	if ev, err := a.CancelExact(key, "gen-1"); err != nil || ev.Disposition != protocol.CancelNoopTerminal {
		t.Fatalf("terminal cancel = %+v, %v; want noop_already_terminal", ev, err)
	}
}

// TestFactoryRequiresHandleAndDir pins the factory contract: config.handle
// is required, validated, and the extension directory must exist.
func TestFactoryRequiresHandleAndDir(t *testing.T) {
	if _, err := Factory(t.Context(), registry.FactoryConfig{Target: "amit"}); err == nil || !strings.Contains(err.Error(), "handle is required") {
		t.Fatalf("err = %v, want handle-required refusal", err)
	}
	root := t.TempDir()
	if _, err := Factory(t.Context(), registry.FactoryConfig{Target: "amit", Root: root, Config: []byte(`{"handle":"agent1"}`)}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want extension-dir-not-found refusal", err)
	}
	// Happy path over a real dir.
	newExtDirAt(t, filepath.Join(root, "agents", "agent1", "extensions", "amit-remote"))
	att, err := Factory(t.Context(), registry.FactoryConfig{Target: "amit", Root: root, Config: []byte(`{"handle":"agent1"}`)})
	if err != nil || att == nil {
		t.Fatalf("Factory = %v, %v; want attachment", att, err)
	}
}

// newExtDirAt creates the contract layout at an explicit path.
func newExtDirAt(t *testing.T, dir string) {
	t.Helper()
	for _, sub := range []string{"requests", "receipts", "events"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
}

// TestSubmitConcurrentMapSafety pins 9a regression: concurrent Submit +
// Lookup + Inspect over the same attachment race-check clean (run under
// -race). The old bug ran consume() outside the mutex and crashed the
// whole companion on concurrent maps.
func TestSubmitConcurrentMapSafety(t *testing.T) {
	a, dir := newTestAttachment(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			key := testKey(fmt.Sprintf("race%d", i))
			ref := clientRef(key)
			writeReceipt(t, dir, ref, "gen-1", fixedNow)
			_, _ = a.Submit(submitReq(key, "x"))
		}(i)
		go func() { defer wg.Done(); _, _ = a.Lookup(testKey("shared"), "gen-1") }()
		go func() { defer wg.Done(); _ = a.Inspect() }()
	}
	wg.Wait()
}

// TestSubscribeDeliversNativeEvents pins the event fan-out: a completed
// event in the stream reaches subscribers through consumeLocked.
func TestSubscribeDeliversNativeEvents(t *testing.T) {
	a, dir := newTestAttachment(t)
	key := testKey("sub")
	ref := clientRef(key)
	seedRequest(t, dir, ref, "")
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	ch := make(chan core.NativeEvent, 1)
	stop := a.Subscribe(func(ev core.NativeEvent) {
		select {
		case ch <- ev:
		default:
		}
	})
	defer stop()
	appendEvents(t, dir, ref, fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"completed","text":"done"}`, ProtocolV1, ref))
	if _, err := a.Lookup(key, "gen-1"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Type != core.EventRunCompleted {
			t.Fatalf("event type = %q, want run_completed", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("no native event delivered to subscriber")
	}
}
