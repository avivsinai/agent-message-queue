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
	evt nostr.Event
	n   int
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

func (a *seqAllocator) peek(key, dir string) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	slot, err := a.prepare(key, dir)
	if err != nil {
		return 0, err
	}
	return slot.next + 1, nil
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
		slot.dir = dir
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

// byteLedger is the process-wide ciphertext budget. 8 MiB per body, 32 MiB
// across bodies.
type byteLedger struct {
	mu    sync.Mutex
	body  map[string]int
	total int
}

func (q *byteLedger) add(body string, n int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.body == nil {
		q.body = map[string]int{}
	}
	q.body[body] += n
	q.total += n
	if q.body[body] < 0 {
		q.total -= q.body[body]
		q.body[body] = 0
	}
	if q.total < 0 {
		q.total = 0
	}
}

func (q *byteLedger) over(body string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.body[body] > bodyQueueBytes || q.total > processQueueMax
}

var queueBytes byteLedger

// limiter caps sends, not frame construction, at 100 frames in any one-second
// window per body. allow checks. commit records a send that returned.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *limiter) allow(body string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.kept(body, now)) < ratePerBody
}

func (l *limiter) commit(body string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[body] = append(l.kept(body, now), now)
}

func (l *limiter) kept(body string, now time.Time) []time.Time {
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	floor := now.Add(-time.Second)
	prev := l.hits[body]
	kept := prev[:0]
	for _, at := range prev {
		if at.After(floor) {
			kept = append(kept, at)
		}
	}
	l.hits[body] = kept
	return kept
}

var processLimit limiter
