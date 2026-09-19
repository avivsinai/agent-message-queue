// Package amit (contract seam): deliver.go implements the adapter side of
// the amit-remote contract v1 paths:
//
//	bridgePath: <AM_ROOT>/agents/<handle>/extensions/amit-remote/
//	requests/<ref>.json   publishRequest — atomic tmp+rename, O_EXCL (§1)
//	receipts/<ref>.json   readReceipt — admission proof (§2)
//	events/<ref>.jsonl    readEvents — terminal evidence, rotation-tolerant (§3, A2)
//	bridge.liveness       liveness — heartbeat freshness (§2)
//
// The adapter writes ONLY requests/<ref>.json (contract §8); every other
// path is read-only. The file names use the sanitized ref (§Identity: ':'
// and '/' replaced by '_').
package amit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SentinelUnpinned is the §4/A4 epoch sentinel: the epoch an attachment
// publishes before it observes its first receipt. Non-empty and
// protocol-valid, so stale-epoch protection cannot be defeated by ""=="".
// Only receipts pin a real generation; the sentinel is never derived from
// the session id or liveness.
const SentinelUnpinned = "unpinned"

// ErrAlreadyDelivered marks the §1 duplicate guard: requests/<ref>.json
// already exists, so the ref was already delivered and must never be
// rewritten or re-sent.
var ErrAlreadyDelivered = errors.New("amit: request already delivered")

// deliverRequest is the §1 request JSON contract.
type deliverRequest struct {
	Ref       string `json:"ref"`
	Text      string `json:"text"`
	DeliverAs string `json:"deliver_as"`
	NotAfter  string `json:"not_after"`
	EpochHint string `json:"epoch_hint,omitempty"` // empty = first contact, no check (§4)
	CreatedAt string `json:"created_at"`
}

// receipt is the §2 receipt JSON contract.
type receipt struct {
	Protocol          string `json:"protocol"`
	Ref               string `json:"ref"`
	SessionGeneration string `json:"session_generation"`
	DeliveredAt       string `json:"delivered_at"`
	PID               int    `json:"pid"`
}

// event is one JSON line of the §3 event stream.
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
// (§3, mirrors the doorbell trace cap). The adapter never trusts a longer
// line than the contract allows.
const maxEventText = 256 * 1024

// livenessRecord is the bridge.liveness JSON shape (the doorbell shape plus
// the amit-remote protocol tag, §9).
type livenessRecord struct {
	Protocol string `json:"protocol"`
	Live     bool   `json:"live"`
	At       string `json:"at"`
	PID      int    `json:"pid"`
	Surface  string `json:"surface"`
}

// heartbeat / freshness: the extension refreshes the heartbeat every 2s and
// a record older than 5s is stale — the doorbell thresholds, one shared
// truth per contract §2.
const (
	heartbeatMaxAge = 5 * time.Second
)

// bridgePath resolves the contract's extension directory for one handle.
func bridgePath(root, handle string) string {
	return filepath.Join(root, "agents", handle, "extensions", "amit-remote")
}

// refSanitize makes a request ref safe as a filename. protocol.EncodeRef
// output is the prefix "amqr1_" plus base32 lowercase — already filename-
// safe — so this is the identity today; it exists as the single named seam
// if the ref grammar ever grows unsafe characters (§Identity).
func refSanitize(ref string) string {
	return ref
}

// bridgeDir is the read/write seam over one extension directory. Production
// New wires the real filesystem; tests may inject stubs per method.
type bridgeDir struct {
	dir string

	// Injectable for tests. Zero funcs fall through to the real files.
	publish func(deliverRequest) error
	receipt func(ref string) (*receipt, error)
	events  func(ref string) ([]event, error)
	live    func(now time.Time) livenessState
}

// livenessState is the classified heartbeat: live with the age, or dead with
// a typed reason ("absent" | "stale" | "malformed" | "unreadable").
type livenessState struct {
	live   bool
	age    time.Duration
	reason string
}

// publishRequest publishes one §1 request: atomic write (unique temp name,
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
		// §1: O_EXCL semantics — an existing request file is a positive
		// pre-send duplicate refusal. Never overwrite.
		return fmt.Errorf("%w: %s", ErrAlreadyDelivered, req.Ref)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("amit: stat request %s: %v", req.Ref, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("amit: prepare requests dir: %v", err)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("amit: marshal request %s: %v", req.Ref, err)
	}
	return writeAtomicNew(path, payload)
}

// writeAtomicNew publishes a COMPLETE payload at path atomically with
// create-new semantics: the fully written and fsynced temp file is
// hard-linked onto the target. link(2) fails with EEXIST when the target
// exists, so the publication is atomic O_EXCL — nothing ever observes a
// partial or empty target file (rename-onto-existing would silently
// overwrite; claim-then-fill would expose an empty file), and a concurrent
// publisher of the same ref loses cleanly (§1).
func writeAtomicNew(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("amit: prepare dir: %v", err)
	}
	tmp, err := os.CreateTemp(dir, ".publish-*")
	if err != nil {
		return fmt.Errorf("amit: temp write: %v", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return fmt.Errorf("amit: temp write: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("amit: temp sync: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("amit: temp close: %v", err)
	}
	// Atomic create-new publication: link fails with EEXIST when another
	// writer already published this ref — the §1 duplicate refusal.
	if err := os.Link(tmpName, path); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: %s", ErrAlreadyDelivered, strings.TrimSuffix(filepath.Base(path), ".json"))
		}
		return fmt.Errorf("amit: publish %s: %v", path, err)
	}
	// link(2) leaves the source name in place — unlink it now (the target
	// holds the published inode); the deferred remove is a no-op backstop.
	if rerr := os.Remove(tmpName); rerr != nil {
		tmpName = "" // target owns the inode; a lingering temp name is harmless
		return fmt.Errorf("amit: unlink temp after publish %s: %v", path, rerr)
	}
	tmpName = "" // published and temp unlinked — the deferred remove is a no-op
	if d, err := os.Open(dir); err != nil {
		return fmt.Errorf("amit: fsync dir: %v", err)
	} else {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// readReceipt reads receipts/<ref>.json (§2). Returns (nil, nil) when the
// receipt is absent. A receipt carrying a foreign protocol string (§9) is
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
		return nil, fmt.Errorf("amit: read receipt %s: %v", ref, err)
	}
	var rc receipt
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("amit: parse receipt %s: %v", ref, err)
	}
	if rc.Protocol != "" && rc.Protocol != ProtocolV1 {
		return nil, fmt.Errorf("amit: receipt %s: unknown protocol %q (want %q)", ref, rc.Protocol, ProtocolV1)
	}
	return &rc, nil
}

// readEvents reads events/<ref>.jsonl (§3), oldest first. A2 rotation
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
		return nil, fmt.Errorf("amit: read events %s: %v", ref, err)
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
			// terminal line is fsynced by the writer (§3), so an unparsed
			// line is never the terminal evidence.
			continue
		}
		if len(ev.Text) > maxEventText {
			ev.Text = ev.Text[:maxEventText]
		}
		if ev.Protocol != "" && ev.Protocol != ProtocolV1 {
			// §9: refuse an unknown protocol string rather than guessing.
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// liveness reads and classifies bridge.liveness (§2): heartbeat freshness
// over the file's mtime (the doorbell pattern), 5s fresh window. The file's
// session_generation advisory is NEVER epoch evidence (§4).
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
	if !rec.Live || rec.PID <= 0 {
		return livenessState{age: age, reason: "malformed"}
	}
	return livenessState{live: true, age: age}
}

// listReceipts returns the ref of every parseable receipt file, oldest
// first (name order = mtime-independent stable order; §5 recovery only
// needs a deterministic sweep). Unparseable refs (junk files) are skipped.
// listReceipts returns every parseable receipt, ordered oldest-first by
// file mtime then name (deterministic; the pinned epoch ends at the newest
// receipt-proven generation, §4/§5). Junk files are skipped, never guessed
// into evidence.
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

// ProtocolV1 is the §9 protocol string. Every receipt/event line carries
// it; the adapter refuses an unknown string rather than guessing.
const ProtocolV1 = "amit:amq-remote:v1"
