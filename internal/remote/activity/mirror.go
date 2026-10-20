package activity

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// CodexSource is the Codex attachment's read-only notification observer,
// ClaudeSource the Claude attachment's parsed-transcript observer, and
// PiSource the pi attachment's bridge activity tail (a distinct method name
// keeps a type switch unambiguous).
type CodexSource interface {
	ObserveNotifications(fn func(codex.Notification)) (stop func())
}

type ClaudeSource interface {
	ObserveActivity(fn func(claude.ActivityNote)) (stop func())
}

type PiSource interface {
	ObservePiActivity(fn func(pi.ActivityNote)) (stop func())
}

// claudeHubs fans one Claude attachment's activity out to every observer.
// The adapter keeps a single activity sink, so the relay export and a local
// mirror of the same target would otherwise replace each other.
var claudeHubs = struct {
	sync.Mutex
	m map[ClaudeSource]*claudeHub
}{m: map[ClaudeSource]*claudeHub{}}

type claudeHub struct {
	stop func()
	next uint64
	fns  map[uint64]func(claude.ActivityNote)
}

// ObserveClaude registers fn for src's activity. The first observer
// registers with the adapter and the last one to stop unregisters.
func ObserveClaude(src ClaudeSource, fn func(claude.ActivityNote)) (stop func()) {
	claudeHubs.Lock()
	defer claudeHubs.Unlock()
	hub := claudeHubs.m[src]
	if hub == nil {
		hub = &claudeHub{fns: map[uint64]func(claude.ActivityNote){}}
		claudeHubs.m[src] = hub
		hub.stop = src.ObserveActivity(func(n claude.ActivityNote) {
			claudeHubs.Lock()
			fns := make([]func(claude.ActivityNote), 0, len(hub.fns))
			for _, f := range hub.fns {
				fns = append(fns, f)
			}
			claudeHubs.Unlock()
			for _, f := range fns {
				f(n)
			}
		})
	}
	hub.next++
	id := hub.next
	hub.fns[id] = fn
	var once sync.Once
	return func() {
		once.Do(func() {
			claudeHubs.Lock()
			defer claudeHubs.Unlock()
			delete(hub.fns, id)
			if len(hub.fns) == 0 && claudeHubs.m[src] == hub {
				delete(claudeHubs.m, src)
				hub.stop()
			}
		})
	}
}

// Mirror projects one native session's activity into plain ACP session
// update objects (the update of a session/update notification) for a local
// client: agent text, tool calls with their results, and plan updates. It
// uses the frame projection and the Codex delta coalescing of the Sink.
// Lifecycle observations and the owner's own prompt are not mirrored: the
// client already shows the prompt it sent.
type Mirror struct {
	ThreadID string
	Send     func(json.RawMessage) error
	// CodexTurn returns the Codex turn of the mirrored request, or "" while
	// it is unknown. Codex items of any other turn on the thread, such as a
	// local turn that starts right after the request, are not mirrored, and
	// items that arrive before the turn is known are dropped.
	CodexTurn func() string

	mu     sync.Mutex
	stream streamState
	turn   string
}

// Codex mirrors one Codex notification for the request's turn on the pinned
// thread.
func (m *Mirror) Codex(n codex.Notification, now time.Time) error {
	fact, ok := projectCodex(m.ThreadID, n)
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.turn == "" && m.CodexTurn != nil {
		m.turn = m.CodexTurn()
	}
	if m.turn == "" || fact.obs.TurnID != m.turn {
		return nil
	}
	return m.stream.accept(fact, now, m.out)
}

// Claude mirrors the blocks of one parsed transcript line of the pinned
// session.
func (m *Mirror) Claude(note claude.ActivityNote) error {
	line := note.Line
	if line.Meta || (line.Type != "user" && line.Type != "assistant") {
		return nil
	}
	session := note.SessionID
	if session == "" {
		session = line.SessionID
	}
	if session == "" || session != m.ThreadID {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, block := range line.Blocks {
		if obs, ok := projectClaudeBlock(session, note.TurnID, line, block); ok {
			if err := m.out(obs); err != nil {
				return err
			}
		}
	}
	return nil
}

// Pi mirrors one pi activity record of the pinned session.
func (m *Mirror) Pi(note pi.ActivityNote) error {
	if note.SessionID == "" || note.SessionID != m.ThreadID {
		return nil
	}
	obs, ok := projectPi(note)
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out(obs)
}

// Tick sends the Codex runs that have been open for coalesceWindow.
func (m *Mirror) Tick(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stream.flushDue(now, m.out)
}

// Flush sends every open Codex run.
func (m *Mirror) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stream.flush(m.out)
}

// Mirror bounds: a tool title or plan entry keeps a prefix of
// mirrorLabelBytes, and an update that still exceeds mirrorUpdateBytes is
// dropped, so one oversized item never ends the stream at the client's
// record limit (ipc.MaxRecordBytes).
const (
	mirrorLabelBytes  = 4 << 10
	mirrorUpdateBytes = 512 << 10
)

// out sends one observation as update objects. Text is split as frames
// split it; a tool result is one update, because the client replaces the
// whole result on each update, so a long one keeps a marked prefix.
func (m *Mirror) out(obs observation) error {
	if obs.Kind != "acp_read" || obs.Update == "user_message_chunk" {
		return nil
	}
	obs.Title = capText(obs.Title, mirrorLabelBytes)
	if obs.Plan != nil {
		plan := make([]planEntry, len(obs.Plan))
		for i, e := range obs.Plan {
			e.Content = capText(e.Content, mirrorLabelBytes)
			plan[i] = e
		}
		obs.Plan = plan
	}
	pieces := []string{obs.Text}
	if obs.Update == "tool_call_update" {
		if len(obs.Text) > maxPlainBytes {
			pieces[0] = capText(obs.Text, maxPlainBytes-len(toolResultBound)) + toolResultBound
		}
	} else if obs.Text != "" {
		pieces = splitRunes(obs.Text, maxPlainBytes)
	}
	for _, piece := range pieces {
		obs.Text = piece
		raw, err := obs.payload()
		if err != nil {
			return err
		}
		var note struct {
			Params struct {
				Update json.RawMessage `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &note); err != nil {
			return err
		}
		if len(note.Params.Update) > mirrorUpdateBytes {
			continue
		}
		if err := m.Send(note.Params.Update); err != nil {
			return err
		}
	}
	return nil
}

// mirrorQueue bounds the hand-off from a native observer to the stream: the
// Codex callback runs on the read pump and must never wait on a client. A
// Codex notification larger than mirrorItemBytes is not copied.
const (
	mirrorQueue     = 256
	mirrorItemBytes = 1 << 20
)

// mirrorGrace is how long a stream keeps observing after its request
// settled. An adapter can report the terminal state just before the activity
// of the same read: the Claude adapter delivers a poll's transcript notes
// after its evidence.
const mirrorGrace = 250 * time.Millisecond

// MirrorHandler serves ipc activity streams from ep: the native activity of
// the request's target while the request runs. The stream follows the
// native session the request is pinned to (else the one attached when it
// starts) and ends when the request is terminal or uncertain, or when the
// target's native session changes. Items that find the queue full are
// dropped; the request itself is never touched.
func MirrorHandler(ep *core.Endpoint) ipc.ActivityHandler {
	return func(ctx context.Context, req ipc.ActivityRequest, native string, send func(json.RawMessage) error) error {
		_, target, _, err := protocol.DecodeRef(req.RequestRef)
		if err != nil {
			return err
		}
		// Only a request this endpoint holds is mirrored: an unknown ref is
		// refused before anything is observed.
		get := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: req.RequestRef}
		if _, err := ep.Handle(get, core.Source{Host: core.LocalHost}); err != nil {
			return err
		}
		if native == "" {
			native = ep.NativeSessionID(target)
		}
		att, ok := ep.AttachmentOf(target)
		if !ok || native == "" {
			return protocol.Refuse(protocol.CodeUnshared, "target %q has no attached native session", target)
		}
		m := &Mirror{ThreadID: native, Send: send, CodexTurn: func() string { return codexTurn(ep, get) }}
		items := make(chan func() error, mirrorQueue)
		offer := func(fn func() error) {
			select {
			case items <- fn:
			default:
			}
		}
		var stop func()
		switch src := att.(type) {
		case CodexSource:
			stop = src.ObserveNotifications(func(n codex.Notification) {
				if !CodexProjected(n.Method) || len(n.Params) > mirrorItemBytes {
					return // ignored before any copy
				}
				n.Params = append(json.RawMessage(nil), n.Params...) // the pump may reuse its buffer
				at := time.Now()
				offer(func() error { return m.Codex(n, at) })
			})
		case ClaudeSource:
			stop = ObserveClaude(src, func(n claude.ActivityNote) { offer(func() error { return m.Claude(n) }) })
		case PiSource:
			stop = src.ObservePiActivity(func(n pi.ActivityNote) { offer(func() error { return m.Pi(n) }) })
		default:
			return protocol.Refuse(protocol.CodeUnsupported, "target %q has no activity seam", target)
		}
		defer stop()

		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		settled := make(chan error, 1)
		go func() {
			_, err := ep.Wait(wctx, req.RequestRef)
			settled <- err
		}()
		tick := time.NewTicker(coalesceWindow)
		defer tick.Stop()
		var grace <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				return nil
			case fn := <-items:
				if err := fn(); err != nil {
					return err
				}
			case now := <-tick.C:
				if ep.NativeSessionID(target) != native {
					return nil
				}
				if err := m.Tick(now); err != nil {
					return err
				}
			case err := <-settled:
				if err != nil {
					return err
				}
				settled = nil
				grace = time.After(mirrorGrace)
			case <-grace:
				stop()
				for len(items) > 0 {
					if err := (<-items)(); err != nil {
						return err
					}
				}
				return m.Flush()
			}
		}
	}
}

// codexTurn is the Codex turn the request runs as, from the run the endpoint
// recorded: the Codex adapter names a run "turn:<turn id>" once turn/start
// answered (codex run.runIDLocked).
func codexTurn(ep *core.Endpoint, get *protocol.Command) string {
	reply, err := ep.Handle(get, core.Source{Host: core.LocalHost})
	rep, ok := reply.(protocol.Reply)
	if err != nil || !ok || rep.Snapshot.NativeRun == nil {
		return ""
	}
	turn, _ := strings.CutPrefix(*rep.Snapshot.NativeRun, "turn:")
	if turn == *rep.Snapshot.NativeRun {
		return ""
	}
	return turn
}
