package activity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type queuedFrame struct {
	sink *Sink
	evt  nostr.Event
	n    int
}

// seqAllocator reserves blocks of 1024 sequence numbers per body and native
// session. Sinks in this process share one slot. A StateDir persists the
// reserved high-water so a later process skips unused values.
type seqAllocator struct {
	mu    sync.Mutex
	slots map[string]*seqSlot
}

type seqSlot struct {
	next   uint64
	hi     uint64
	dir    string
	failed error
}

func (a *seqAllocator) commit(key, dir string) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	slot, err := a.prepare(key, dir)
	if err != nil {
		return 0, err
	}
	if slot.next == slot.hi {
		if slot.hi > math.MaxUint64-seqBlock {
			slot.failed = fmt.Errorf("activity sequence is exhausted")
			return 0, slot.failed
		}
		nextHi := slot.hi + seqBlock
		if slot.dir != "" {
			if err := writeSeqHi(slot.dir, key, nextHi); err != nil {
				slot.failed = err
				return 0, err
			}
		}
		slot.hi = nextHi
	}
	slot.next++
	return slot.next, nil
}

func (a *seqAllocator) prepare(key, dir string) (*seqSlot, error) {
	if a.slots == nil {
		a.slots = map[string]*seqSlot{}
	}
	slot := a.slots[key]
	if slot == nil {
		slot = &seqSlot{dir: dir}
		if dir != "" {
			hi, err := readSeqHi(dir, key)
			if err != nil {
				slot.failed = err
				a.slots[key] = slot
				return nil, err
			}
			slot.next, slot.hi = hi, hi
		}
		a.slots[key] = slot
	} else if slot.dir == "" && dir != "" {
		hi, err := readSeqHi(dir, key)
		if err != nil {
			slot.failed = err
			return nil, err
		}
		// Equal ceilings are still two reservations of the same block: a
		// process-only allocator and a durable one can both stop at 1024.
		// The next number has to pass the persisted reservation.
		if hi >= slot.hi && hi > 0 {
			slot.next, slot.hi = hi, hi
		}
		slot.dir = dir
		if slot.hi > 0 {
			if err := writeSeqHi(dir, key, slot.hi); err != nil {
				slot.failed = err
				return nil, err
			}
		}
	}
	if slot.failed != nil {
		return nil, slot.failed
	}
	return slot, nil
}

func seqFile(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".seq"
}

func readSeqHi(dir, key string) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(dir, seqFile(key)))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	hi, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("activity sequence file: %w", err)
	}
	return hi, nil
}

func writeSeqHi(dir, key string, hi uint64) error {
	_, err := fsq.WriteFileAtomic(dir, seqFile(key), []byte(strconv.FormatUint(hi, 10)), 0o600)
	return err
}

var processSeq seqAllocator

// liveQueue is the shared body and process frame budget. Age, count, and
// bytes are owned here, not on one sink. Close releases that sink's charges.
type liveQueue struct {
	mu           sync.Mutex
	body         map[string][]queuedFrame
	bodyBytes    map[string]int
	processBytes int
}

func (q *liveQueue) push(sink *Sink, body string, evt nostr.Event, n int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.evictStaleLocked(sink.now())
	if q.body == nil {
		q.body = map[string][]queuedFrame{}
		q.bodyBytes = map[string]int{}
	}
	q.body[body] = append(q.body[body], queuedFrame{sink: sink, evt: evt, n: n})
	q.bodyBytes[body] += n
	q.processBytes += n
	q.trimLocked(body)
}

func (q *liveQueue) trimLocked(body string) {
	for q.overLocked(body) && len(q.body[body]) > 0 {
		q.dropLocked(body, 0)
	}
}

func (q *liveQueue) overLocked(body string) bool {
	return len(q.body[body]) > ringCap || q.bodyBytes[body] > bodyQueueBytes || q.processBytes > processQueueMax
}

func (q *liveQueue) dropLocked(body string, i int) {
	item := q.takeLocked(body, i)
	item.sink.noteDrop()
}

// takeLocked removes one queued frame and drops the backing-array slot.
// An empty body is removed from the process maps so a closed sink does not
// keep ciphertext or the sink pointer alive.
func (q *liveQueue) takeLocked(body string, i int) queuedFrame {
	items := q.body[body]
	item := items[i]
	copy(items[i:], items[i+1:])
	items[len(items)-1] = queuedFrame{}
	items = items[:len(items)-1]
	if len(items) == 0 {
		delete(q.body, body)
	} else {
		q.body[body] = items
	}
	q.bodyBytes[body] -= item.n
	q.processBytes -= item.n
	if q.bodyBytes[body] <= 0 {
		delete(q.bodyBytes, body)
	}
	if q.processBytes < 0 {
		q.processBytes = 0
	}
	return item
}

func (q *liveQueue) evictStaleLocked(now time.Time) {
	for body, items := range q.body {
		for i := 0; i < len(items); {
			if staleFrame(items[i].evt, now) {
				q.dropLocked(body, i)
				items = q.body[body]
				continue
			}
			i++
		}
	}
}

func (q *liveQueue) hasLocked(sink *Sink, body string) bool {
	for _, item := range q.body[body] {
		if item.sink == sink {
			return true
		}
	}
	return false
}

func (q *liveQueue) popLocked(sink *Sink, body string) (queuedFrame, bool) {
	items := q.body[body]
	for i, item := range items {
		if item.sink != sink {
			continue
		}
		return q.takeLocked(body, i), true
	}
	return queuedFrame{}, false
}

func (q *liveQueue) release(sink *Sink, body string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.body[body]
	for i := 0; i < len(items); {
		if items[i].sink == sink {
			q.dropLocked(body, i)
			items = q.body[body]
			continue
		}
		i++
	}
}

func staleFrame(evt nostr.Event, now time.Time) bool {
	return now.Unix()-int64(evt.CreatedAt) > int64(maxPendingAge/time.Second)
}

var live liveQueue

// limiter is the process-wide cap of 100 attempted sends in any one-second
// window per body. reserve counts an attempt, including time spent in Publish.
// finish keeps that charge after an ambiguous result.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]rateHit
}

type rateHit struct {
	at       time.Time
	inflight bool
}

func (l *limiter) reserve(body string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.kept(body, now)
	if len(kept) >= ratePerBody {
		l.hits[body] = kept
		return false
	}
	l.hits[body] = append(kept, rateHit{at: now, inflight: true})
	return true
}

func (l *limiter) finish(body string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	hits := l.hits[body]
	for i := range hits {
		if hits[i].inflight {
			hits[i].inflight = false
			hits[i].at = now
			return
		}
	}
}

func (l *limiter) kept(body string, now time.Time) []rateHit {
	if l.hits == nil {
		l.hits = map[string][]rateHit{}
	}
	floor := now.Add(-time.Second)
	prev := l.hits[body]
	kept := make([]rateHit, 0, len(prev))
	for _, hit := range prev {
		if hit.inflight || hit.at.After(floor) {
			kept = append(kept, hit)
		}
	}
	return kept
}

var processLimit limiter
