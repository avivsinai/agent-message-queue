// Package pi (bridge seam): deliver.go implements the adapter side of the
// pi-bridge protocol v1 paths:
//
//	bridgePath: <AM_ROOT>/agents/<handle>/extensions/pi-bridge/ (see wireNames)
//	requests/<ref>.json   publishRequest — atomic tmp+rename, O_EXCL (protocol: requests)
//	receipts/<ref>.json   readReceipt — admission proof (protocol: receipts)
//	events/<ref>.jsonl    readEvents — terminal evidence, rotation-tolerant (protocol: events)
//	bridge.liveness       liveness — heartbeat freshness (protocol: liveness)
//
// The adapter writes ONLY requests/<ref>.json (protocol: ownership); every other
// path is read-only. The file names use the sanitized ref, with ':'
// and '/' replaced by '_' (protocol: identity and layout).
package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// SentinelUnpinned is the epoch sentinel: the epoch an attachment
// publishes before it observes its first receipt while no live generation
// is known. Non-empty and protocol-valid, so stale-epoch protection cannot
// be defeated by ""=="". It addresses no session generation, so a submit
// under it is refused stale_epoch. Only receipts pin a real generation.
const SentinelUnpinned = "unpinned"

// addressPrefix starts an address epoch: `unpinned.<generation>` names the
// live generation Inspect observed before the first receipt. It binds the
// caller's submit to that generation without pinning it (protocol: session
// generation and epoch).
const addressPrefix = SentinelUnpinned + "."

// addressEpoch returns the address epoch for a live generation, or "" when
// the generation cannot form a protocol-valid epoch.
func addressEpoch(gen string) string {
	if !protocol.ValidEpoch(gen) {
		return ""
	}
	e := addressPrefix + gen
	if !protocol.ValidEpoch(e) {
		return ""
	}
	return e
}

// addressedGeneration returns the generation an address epoch names.
func addressedGeneration(epoch string) (string, bool) {
	gen, ok := strings.CutPrefix(epoch, addressPrefix)
	if !ok || addressEpoch(gen) != epoch {
		return "", false
	}
	return gen, true
}

// MinBridgeRevision is the lowest extension bridge_revision that implements
// the request-ownership, refusal, and recovery rules this adapter relies
// on. A live bridge without the marker, or below it, gets no new
// submissions (protocol: bridge revision).
const MinBridgeRevision = 2

// ErrAlreadyDelivered marks the duplicate guard: requests/<ref>.json
// already exists, so the ref was already delivered and must never be
// rewritten or re-sent.
var ErrAlreadyDelivered = errors.New("pi: request already delivered")

// ErrForeignEventStream marks the protocol-string refusal of an event stream carrying a
// foreign protocol string. It is typed so tests and callers can recognise
// the refusal via errors.Is (review 816-r4 P1). Lifting rules live in
// applyObservationLocked and are protocol-uniform: the refusal survives an
// absent-or-unreadable log (rotation, truncation) and lifts only when a
// present stream validates as v1 end to end.
var ErrForeignEventStream = errors.New("pi: foreign-protocol event stream")

// deliverRequest is the request JSON contract.
type deliverRequest struct {
	Ref       string `json:"ref"`
	Text      string `json:"text"`
	DeliverAs string `json:"deliver_as"`
	NotAfter  string `json:"not_after"`
	EpochHint string `json:"epoch_hint,omitempty"` // the addressed session generation; the adapter never sends it empty (protocol: session generation and epoch)
	CreatedAt string `json:"created_at"`
}

// receipt is the receipt JSON contract.
type receipt struct {
	Protocol          string `json:"protocol"`
	Ref               string `json:"ref"`
	SessionGeneration string `json:"session_generation"`
	DeliveredAt       string `json:"delivered_at"`
	PID               int    `json:"pid"`
}

// event is one JSON line of the event stream.
type event struct {
	Protocol string `json:"protocol"`
	Ref      string `json:"ref"`
	Event    string `json:"event"`
	Text     string `json:"text,omitempty"`
	Error    string `json:"error,omitempty"`
	Reason   string `json:"reason,omitempty"`
	At       string `json:"at,omitempty"`
}

// maxEventText bounds one event line's text to the contract's 256 KiB cap
// (protocol: events; mirrors the doorbell trace cap). The adapter never trusts a longer
// line than the contract allows.
const maxEventText = 256 * 1024

// livenessRecord is the bridge.liveness JSON shape (the doorbell shape plus
// the bridge protocol tag; protocol: protocol string).
type livenessRecord struct {
	Protocol string `json:"protocol"`
	Live     bool   `json:"live"`
	At       string `json:"at"`
	PID      int    `json:"pid"`
	Surface  string `json:"surface"`
	// SessionGeneration is advisory: /new, reload, and fork keep the pid but
	// change the generation, so it may drop a pin that no longer matches,
	// never set one (protocol: session generation and epoch).
	SessionGeneration string `json:"session_generation,omitempty"`
	// BridgeRevision is the extension's implementation revision. 0 means
	// the record carries none (protocol: bridge revision).
	BridgeRevision int `json:"bridge_revision,omitempty"`
}

// heartbeat / freshness: the extension refreshes the heartbeat every 2s and
// a record older than 5s is stale — the doorbell thresholds, one shared
// truth (protocol: liveness).
const (
	heartbeatMaxAge = 5 * time.Second
)

// bridgePath resolves the bridge's extension directory for one handle.
func bridgePath(root, handle string, w wireNames) string {
	return filepath.Join(root, "agents", handle, "extensions", w.dir)
}

// refSanitize makes a request ref safe as a filename. protocol.EncodeRef
// output is the prefix "amqr1_" plus base32 lowercase — already filename-
// safe — so this is the identity today; it exists as the single named seam
// if the ref grammar ever grows unsafe characters (protocol: identity and layout).
func refSanitize(ref string) string {
	return ref
}

// bridgeDir is the read/write seam over one extension directory. Production
// New wires the real filesystem; tests may inject stubs per method.
type bridgeDir struct {
	dir string
	// names are the wire names of this bridge generation; every receipt,
	// event line, and liveness record must carry names.protocol (protocol: protocol string).
	names wireNames

	// Injectable for tests. Zero funcs fall through to the real files.
	publish func(deliverRequest) error
	receipt func(ref string) (*receipt, error)
	events  func(ref string) ([]event, error)
	live    func(now time.Time) livenessState
}

// livenessState is the classified heartbeat: live with the age, or dead with
// a typed reason ("absent" | "stale" | "malformed" | "unreadable").
type livenessState struct {
	live     bool
	age      time.Duration
	reason   string
	pid      int    // the live bridge process (set only when live)
	gen      string // the live bridge's advisory generation ("" = not published)
	revision int    // the live bridge's bridge_revision (0 = not published)
}

// publishRequest publishes one request: atomic write (unique temp name,
// fsync file, rename, fsync dir), create-new via the rename over an
// existing file being refused first. The rename is the publication
// boundary — nothing polls a partial file.
func (b bridgeDir) publishRequest(req deliverRequest) error {
	if b.publish != nil {
		return b.publish(req)
	}
	dir := filepath.Join(b.dir, "requests")
	path := filepath.Join(dir, refSanitize(req.Ref)+".json")
	if _, err := os.Stat(path); err == nil {
		// O_EXCL semantics — an existing request file is a positive
		// pre-send duplicate refusal. Never overwrite.
		return fmt.Errorf("%w: %s", ErrAlreadyDelivered, req.Ref)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("pi: stat request %s: %v", req.Ref, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pi: prepare requests dir: %v", err)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("pi: marshal request %s: %v", req.Ref, err)
	}
	return writeAtomicNew(path, payload)
}

// writeAtomicNew publishes a COMPLETE payload at path atomically with
// create-new semantics: the fully written and fsynced temp file is
// hard-linked onto the target. link(2) fails with EEXIST when the target
// exists, so the publication is atomic O_EXCL — nothing ever observes a
// partial or empty target file (rename-onto-existing would silently
// overwrite; claim-then-fill would expose an empty file), and a concurrent
// publisher of the same ref loses cleanly (protocol: requests).
func writeAtomicNew(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pi: prepare dir: %v", err)
	}
	tmp, err := os.CreateTemp(dir, ".publish-*")
	if err != nil {
		return fmt.Errorf("pi: temp write: %v", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pi: temp write: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pi: temp sync: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pi: temp close: %v", err)
	}
	// Atomic create-new publication: link fails with EEXIST when another
	// writer already published this ref — the duplicate refusal.
	if err := os.Link(tmpName, path); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: %s", ErrAlreadyDelivered, strings.TrimSuffix(filepath.Base(path), ".json"))
		}
		return fmt.Errorf("pi: publish %s: %v", path, err)
	}
	// link(2) leaves the source name in place — unlink it now (the target
	// holds the published inode); the deferred remove is a no-op backstop.
	if rerr := os.Remove(tmpName); rerr != nil {
		tmpName = "" // target owns the inode; a lingering temp name is harmless
		return fmt.Errorf("pi: unlink temp after publish %s: %v", path, rerr)
	}
	tmpName = "" // published and temp unlinked — the deferred remove is a no-op
	if d, err := os.Open(dir); err != nil {
		return fmt.Errorf("pi: fsync dir: %v", err)
	} else {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// readReceipt reads receipts/<ref>.json (protocol: receipts). Returns (nil, nil) when the
// receipt is absent. A receipt carrying a foreign protocol string (protocol: protocol string) is
// refused, never guessed into evidence.
func (b bridgeDir) readReceipt(ref string) (*receipt, error) {
	if b.receipt != nil {
		return b.receipt(ref)
	}
	data, err := os.ReadFile(filepath.Join(b.dir, "receipts", refSanitize(ref)+".json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pi: read receipt %s: %v", ref, err)
	}
	var rc receipt
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("pi: parse receipt %s: %v", ref, err)
	}
	if rc.Protocol != "" && rc.Protocol != b.names.protocol {
		return nil, fmt.Errorf("pi: receipt %s: unknown protocol %q (want %q)", ref, rc.Protocol, b.names.protocol)
	}
	return &rc, nil
}

// readEvents reads events/<ref>.jsonl (protocol: events), oldest first. Rotation
// tolerance: only the LAST rotation segment is read (the current file); a
// missing file yields no events, never an error.
func (b bridgeDir) readEvents(ref string) ([]event, error) {
	if b.events != nil {
		return b.events(ref)
	}
	data, err := os.ReadFile(filepath.Join(b.dir, "events", refSanitize(ref)+".jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pi: read events %s: %v", ref, err)
	}
	var out []event
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// A partial trailing line (a crash mid-append) is skipped: the
			// terminal line is fsynced by the writer (protocol: events), so an unparsed
			// line is never the terminal evidence.
			continue
		}
		if len(ev.Text) > maxEventText {
			ev.Text = ev.Text[:maxEventText]
		}
		if ev.Protocol != "" && ev.Protocol != b.names.protocol {
			// Refuse an unknown protocol string rather than guessing.
			// A v1 receipt + v2-only event stream must NOT read as "no
			// events" (recovery row 3 would map that to confirmed-running,
			// hiding a terminal state); the whole stream is refused so the
			// run keeps its current state and surfaces the error.
			return nil, fmt.Errorf("%w: %s: unknown protocol %q (want %q)", ErrForeignEventStream, ref, ev.Protocol, b.names.protocol)
		}
		out = append(out, ev)
	}
	return out, nil
}

// liveness reads and classifies bridge.liveness (protocol: liveness): heartbeat freshness
// over the file's mtime (the doorbell pattern), 5s fresh window. The file's
// session_generation advisory never pins (protocol: session generation and epoch); it only drops a stale pin.
func (b bridgeDir) liveness(now time.Time) livenessState {
	if b.live != nil {
		return b.live(now)
	}
	path := filepath.Join(b.dir, "bridge.liveness")
	st, err := os.Stat(path)
	if err != nil {
		return livenessState{reason: "absent"}
	}
	age := now.Sub(st.ModTime())
	if age < 0 {
		age = 0
	}
	if age > heartbeatMaxAge {
		return livenessState{age: age, reason: "stale"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return livenessState{age: age, reason: "unreadable"}
	}
	var rec livenessRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return livenessState{age: age, reason: "malformed"}
	}
	if rec.Protocol != "" && rec.Protocol != b.names.protocol {
		// A foreign-protocol bridge is not OUR bridge. Treating it as
		// live would let the adapter write requests into a seam owned by a
		// different protocol — refuse the pre-gate.
		return livenessState{age: age, reason: "foreign protocol " + rec.Protocol}
	}
	if !rec.Live || rec.PID <= 0 {
		return livenessState{age: age, reason: "malformed"}
	}
	return livenessState{live: true, age: age, pid: rec.PID, gen: rec.SessionGeneration, revision: rec.BridgeRevision}
}

// listReceipts returns every parseable receipt, ordered oldest-first by
// file mtime then name (deterministic; recovery only needs a
// deterministic sweep and the pinned epoch ends at the last receipt
// applied). Unparseable/junk files are skipped, never guessed into
// evidence.
func (b bridgeDir) listReceipts() []receipt {
	entries, err := os.ReadDir(filepath.Join(b.dir, "receipts"))
	if err != nil {
		return nil
	}
	type named struct {
		name  string
		mtime time.Time
	}
	files := make([]named, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, named{name: strings.TrimSuffix(e.Name(), ".json"), mtime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mtime.Equal(files[j].mtime) {
			return files[i].mtime.Before(files[j].mtime)
		}
		return files[i].name < files[j].name
	})
	out := make([]receipt, 0, len(files))
	for _, f := range files {
		rc, err := b.readReceipt(f.name)
		if err != nil || rc == nil {
			continue
		}
		// The protocol ref is exactly the filename: protocol.EncodeRef
		// output (prefix "amqr1_" + base32 lowercase) is already a safe
		// filename, and refSanitize is the identity over it.
		rc.Ref = f.name
		out = append(out, *rc)
	}
	return out
}

// listEventRefs returns the ref of every event stream file, sorted by name.
// Recovery uses it to find refused refs that have no receipt.
func (b bridgeDir) listEventRefs() []string {
	entries, err := os.ReadDir(filepath.Join(b.dir, "events"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".jsonl"))
	}
	sort.Strings(out)
	return out
}

// ProtocolV1 is the protocol string of kind "pi". Every receipt/event
// line carries it; the adapter refuses an unknown string rather than
// guessing.
const ProtocolV1 = "amq:pi-bridge:v1"

// wireNames is the set of names one bridge generation puts on disk and on
// the wire: the extension directory, the protocol string, and the run-id
// prefix the endpoint persists as the native run.
type wireNames struct {
	dir       string
	protocol  string
	runPrefix string
}

// piWire is the wire of kind "pi".
var piWire = wireNames{dir: "pi-bridge", protocol: ProtocolV1, runPrefix: "pi:"}
