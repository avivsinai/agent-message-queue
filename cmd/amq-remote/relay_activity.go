package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/activity"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/sharestate"
)

// notificationSource is the Codex attachment's read-only notification
// observer; claudeSource is the Claude attachment's parsed-transcript
// observer. Activity export reads either one.
type notificationSource interface {
	ObserveNotifications(fn func(codex.Notification)) (stop func())
}

type claudeSource interface {
	ObserveActivity(fn func(claude.ActivityNote)) (stop func())
}

// activityItem is one queued native observation: exactly one field is set.
type activityItem struct {
	codex  *codex.Notification
	claude *claude.ActivityNote
}

// blockSize is one Claude block's in-memory size; the note retains its whole
// backing array, capacity included.
var blockSize = int64(unsafe.Sizeof(claude.TranscriptBlock{}))

// size is the bytes an item retains, for the queue's byte budget: for a
// Claude note every string it holds plus the block array's full allocation,
// cap times element size (codex #872 r1, r2).
func (it activityItem) size() int64 {
	if it.codex != nil {
		return int64(len(it.codex.Params)) // the method is one of three allowlisted names
	}
	n := it.claude
	total := int64(len(n.SessionID) + len(n.TurnID) + len(n.Line.Type) + len(n.Line.SessionID) + len(n.Line.UUID))
	total += int64(cap(n.Line.Blocks)) * blockSize
	for _, b := range n.Line.Blocks {
		total += int64(len(b.Type) + len(b.Text) + len(b.Name) + len(b.ID))
	}
	return total
}

// observeFunc registers offer as the attachment's observer and returns stop.
type observeFunc func(offer func(activityItem)) (stop func())

// observerOf returns the activity observer of an attachment, or nil.
func observerOf(att any) observeFunc {
	switch src := att.(type) {
	case notificationSource:
		return func(offer func(activityItem)) func() {
			return src.ObserveNotifications(func(n codex.Notification) {
				if !projectedMethods[n.Method] {
					return // ignored before any copy
				}
				offer(activityItem{codex: &n})
			})
		}
	case claudeSource:
		return func(offer func(activityItem)) func() {
			return src.ObserveActivity(func(n claude.ActivityNote) { offer(activityItem{claude: &n}) })
		}
	}
	return nil
}

// activityShare is one share's activity export (611.17 slice 3): Codex
// notifications for the approved native thread become NIP-44 kind 24200
// frames to the owner. Export runs only while the target's attached native
// session is the pinned one (relay design §2); it carries no control.
type activityShare struct {
	share    manifest.Share
	body     nostr.SecretKey
	owner    nostr.PubKey
	stateDir string

	mu    sync.Mutex
	state string // exporting, or why not
}

// activityDrainTick is how often queued frames are drained, and the native
// fence re-checked.
var activityDrainTick = 250 * time.Millisecond

// activityPublishTimeout bounds how long one drain pass waits on the relay.
// It does not bound sequence I/O or encoding, so shutdown can take longer
// while a directory sync is slow.
const activityPublishTimeout = 10 * time.Second

func (as *activityShare) set(state string) {
	as.mu.Lock()
	as.state = state
	as.mu.Unlock()
}

func (as *activityShare) view() string {
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.state
}

// newActivityShare loads a share's body for activity export.
func newActivityShare(root, stateDir string, sh manifest.Share) (*activityShare, error) {
	creds, err := sharestate.Load(root, sh.Session)
	if err != nil {
		return nil, err
	}
	owner, err := nostr.PubKeyFromHex(sh.OwnerPubKey)
	if err != nil {
		return nil, err
	}
	return &activityShare{
		share: sh, body: nostr.SecretKey(creds.Body.Secret()), owner: owner,
		stateDir: filepath.Join(stateDir, "activity", sh.Session), state: "configured",
	}, nil
}

// runActivity is one authenticated connection's activity export. It
// observes the target's notifications only while the native fence holds,
// drains the sink on a tick, and stops observing the moment the attached
// session is no longer the approved one. It returns when the connection or
// ctx ends.
func (as *activityShare) runActivity(ctx context.Context, conn *relay.Conn, identity func(string) string, attachment func(string) (core.Attachment, bool), warn io.Writer) {
	target, pin := as.share.Target, as.share.NativeSessionID
	tick := time.NewTicker(activityDrainTick)
	defer tick.Stop()
	for {
		if identity(target) != pin {
			as.set("closed: the attached session is not the one approved for sharing")
		} else if att, ok := attachment(target); !ok {
			as.set("closed: target is not attached")
		} else if observe := observerOf(att); observe == nil {
			as.set("refused: this target's adapter has no activity seam")
		} else {
			as.export(ctx, conn, observe, identity, warn)
		}
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case <-tick.C:
		}
	}
}

// Queue bounds (codex #871 r2): the read-pump callback admits a
// notification only if its method is one the projection uses, the queue has
// room, the payload is at most activityItemBytes, and the queued payloads
// stay within activityQueueBytes (the sink's per-body budget). Everything
// else is dropped before any copy.
const (
	activityQueue      = 256
	activityItemBytes  = 1 << 20
	activityQueueBytes = 8 << 20
)

// projectedMethods are the Codex notifications the activity projection maps.
var projectedMethods = map[string]bool{"turn/started": true, "item/completed": true, "turn/completed": true}

// activityInbox is the bounded hand-off from the native observer to the
// worker.
type activityInbox struct {
	q       chan activityItem
	bytes   atomic.Int64 // payload bytes admitted and not yet taken
	dropped atomic.Uint64
}

func newActivityInbox() *activityInbox {
	return &activityInbox{q: make(chan activityItem, activityQueue)}
}

// offer admits it without blocking, or drops it before any copy.
func (b *activityInbox) offer(it activityItem) bool {
	size := it.size()
	if size > activityItemBytes || len(b.q) == cap(b.q) {
		b.dropped.Add(1)
		return false
	}
	if b.bytes.Add(size) > activityQueueBytes {
		b.bytes.Add(-size)
		b.dropped.Add(1)
		return false
	}
	if it.codex != nil {
		n := *it.codex
		n.Params = append(json.RawMessage(nil), n.Params...) // the pump may reuse its buffer
		it.codex = &n
	}
	select {
	case b.q <- it:
		return true
	default:
		b.bytes.Add(-size)
		b.dropped.Add(1)
		return false
	}
}

// taken releases an item's bytes once the worker has it.
func (b *activityInbox) taken(it activityItem) { b.bytes.Add(-it.size()) }

// errActivityFenced ends a publication whose native session is no longer
// the pinned one.
var errActivityFenced = errors.New("the attached session is not the one approved for sharing")

// export observes and drains until the fence fails or the connection ends.
//
// The observer callback runs on the Codex read pump, so it only copies the
// notification into a bounded queue and never blocks, encodes, does I/O or
// calls the endpoint (codex #871 r1 #2). A worker applies the native fence
// and feeds the sink. Every publication re-checks the fence, and a mismatch
// discards the rest of the queue (#1). Shutdown stops the observer and waits
// for the worker before the sink is released; only the worker feeds the
// sink, so no late callback acts after cleanup (#3).
func (as *activityShare) export(ctx context.Context, conn *relay.Conn, observe observeFunc, identity func(string) string, warn io.Writer) {
	target, pin := as.share.Target, as.share.NativeSessionID
	fenced := func() bool { return identity(target) != pin }
	sink := &activity.Sink{ThreadID: pin, Body: as.body, Owner: as.owner, StateDir: as.stateDir,
		Publish: func(pctx context.Context, evt nostr.Event) error {
			if fenced() {
				return errActivityFenced
			}
			return conn.Publish(pctx, evt)
		}}

	// Only the worker touches the sink. A callback that runs after cleanup
	// can at most fill this buffer, which no one reads again.
	inbox := newActivityInbox()
	// drainMu serializes every path that pops and publishes frames: one
	// drainer at a time, from dequeue through publication. The native
	// callback never takes it.
	var drainMu sync.Mutex
	stop := observe(func(it activityItem) { inbox.offer(it) })

	wctx, cancelWorker := context.WithCancel(ctx)
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		for {
			select {
			case <-wctx.Done():
				return
			case it := <-inbox.q:
				inbox.taken(it)
				if fenced() {
					continue // never export a replacement session's activity
				}
				var err error
				if it.codex != nil {
					err = sink.Enqueue(*it.codex)
				} else {
					// AcceptParsed also drains, through the fenced Publish;
					// drainMu keeps it and the tick's Drain from publishing
					// out of order (codex #872 r1).
					pctx, cancel := context.WithTimeout(wctx, activityPublishTimeout)
					drainMu.Lock()
					err = sink.AcceptParsed(pctx, *it.claude)
					drainMu.Unlock()
					cancel()
				}
				if err != nil && !errors.Is(err, errActivityFenced) {
					say(warn, "relay share %s: activity: %v", as.share.Session, err)
				}
			}
		}
	}()
	defer func() {
		stop()
		cancelWorker()
		worker.Wait()
		sink.Close()
		if n := inbox.dropped.Load(); n > 0 {
			say(warn, "relay share %s: activity: %d notification(s) dropped at a full queue", as.share.Session, n)
		}
	}()

	as.set("exporting")
	tick := time.NewTicker(activityDrainTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case <-tick.C:
		}
		if fenced() {
			as.set("closed: " + errActivityFenced.Error())
			return
		}
		pctx, cancel := context.WithTimeout(ctx, activityPublishTimeout)
		drainMu.Lock()
		err := sink.Drain(pctx)
		drainMu.Unlock()
		cancel()
		switch {
		case errors.Is(err, errActivityFenced):
			as.set("closed: " + errActivityFenced.Error())
			return // the deferred Close discards what is still queued
		case err != nil:
			as.set(fmt.Sprintf("publish_pending: %s", relay.Category(err)))
		default:
			as.set("exporting")
		}
	}
}
