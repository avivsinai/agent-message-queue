// Package activity projects native harness notifications into kind 24200
// telemetry frames. It does not run a relay connection. The caller publishes
// each signed event through a seam with the same shape as relay.Conn.Publish.
package activity

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"

	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

const (
	// KindTelemetry is the NIP-AO activity event kind.
	KindTelemetry nostr.Kind = 24200

	// MaxFrameBytes is the serialized WebSocket EVENT frame ceiling.
	MaxFrameBytes = 64 * 1024

	// maxPlainBytes is the plaintext cap. A larger observation is split on a
	// UTF-8 boundary before encryption, then again if the frame still exceeds
	// MaxFrameBytes.
	maxPlainBytes = 48000

	ringCap         = 800
	bodyQueueBytes  = 8 << 20
	processQueueMax = 32 << 20
	maxPendingAge   = 30 * time.Second
	ratePerBody     = 100
	seqBlock        = 1024
)

// Publish is the relay send seam. It matches relay.Conn.Publish.
// A non-nil error is an ambiguous or refused send. The sink does not call
// Publish again for that event.
type Publish func(ctx context.Context, evt nostr.Event) error

// Sink turns Codex notifications for one pinned native thread into signed
// kind 24200 frames.
type Sink struct {
	ThreadID string
	Body     nostr.SecretKey
	Owner    nostr.PubKey
	Publish  Publish
	Now      func() time.Time
	// StateDir persists the sequence high-water for this body and native
	// session. Empty keeps the reservation in this process only.
	StateDir string

	sawSession bool
	ring       []queuedFrame
	dropped    uint64
}

// Drops is the number of queued frames this sink discarded for age or budget.
func (s *Sink) Drops() uint64 {
	return s.dropped
}

// Enqueue copies one notification into the bounded frame queue and returns
// without publishing. This is the native callback: it does not wait on Publish.
func (s *Sink) Enqueue(n codex.Notification) error {
	_, err := s.enqueue(n)
	return err
}

// Accept maps one Codex notification, queues the frames, and publishes those
// the process-wide per-body rate allows. A notification for another thread
// is ignored. An ambiguous publish is not retried.
func (s *Sink) Accept(ctx context.Context, n codex.Notification) error {
	if s.Publish == nil {
		return fmt.Errorf("activity publish is not set")
	}
	queued, err := s.enqueue(n)
	if err != nil || !queued {
		return err
	}
	return s.flush(ctx)
}

func (s *Sink) enqueue(n codex.Notification) (bool, error) {
	obs, ok := projectCodex(s.ThreadID, n)
	if !ok {
		return false, nil
	}
	if !s.sawSession && s.ThreadID != "" {
		ready := observation{Kind: "session_resolved", SessionID: s.ThreadID, At: obs.At}
		if err := s.queue(ready); err != nil {
			return false, err
		}
		s.sawSession = true
	}
	if err := s.queue(obs); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Sink) queue(obs observation) error {
	events, err := s.frames(obs)
	if err != nil {
		return err
	}
	for _, evt := range events {
		if err := s.push(evt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sink) frames(obs observation) ([]nostr.Event, error) {
	if obs.Text == "" {
		return s.fit(obs)
	}
	var out []nostr.Event
	for _, piece := range splitRunes(obs.Text, maxPlainBytes) {
		part := obs
		part.Text = piece
		events, err := s.fit(part)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

func (s *Sink) fit(obs observation) ([]nostr.Event, error) {
	if obs.At.IsZero() {
		obs.At = s.now()
	}
	probe := obs
	seq, err := s.peekSeq()
	if err != nil {
		return nil, err
	}
	probe.Seq = seq
	evt, err := s.build(probe)
	if err != nil {
		return nil, err
	}
	n, err := frameLen(evt)
	if err != nil {
		return nil, err
	}
	if n > MaxFrameBytes {
		left, right := half(obs.Text)
		if left == "" || right == "" {
			return nil, fmt.Errorf("activity frame is %d bytes", n)
		}
		a, err := s.fit(observationText(obs, left))
		if err != nil {
			return nil, err
		}
		b, err := s.fit(observationText(obs, right))
		if err != nil {
			return nil, err
		}
		return append(a, b...), nil
	}
	obs.Seq, err = s.commitSeq()
	if err != nil {
		return nil, err
	}
	evt, err = s.build(obs)
	if err != nil {
		return nil, err
	}
	return []nostr.Event{evt}, nil
}

func observationText(obs observation, text string) observation {
	obs.Text = text
	return obs
}

func (s *Sink) build(obs observation) (nostr.Event, error) {
	plain, err := obs.marshal()
	if err != nil {
		return nostr.Event{}, err
	}
	key, err := nip44.GenerateConversationKey(s.Owner, s.Body)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := nip44.Encrypt(plain, key)
	if err != nil {
		return nostr.Event{}, err
	}
	evt := nostr.Event{
		CreatedAt: nostr.Timestamp(s.now().Unix()),
		Kind:      KindTelemetry,
		Tags: nostr.Tags{
			{"p", s.Owner.Hex()},
			{"agent", s.Body.Public().Hex()},
			{"frame", "telemetry"},
		},
		Content: content,
	}
	if err := evt.Sign(s.Body); err != nil {
		return nostr.Event{}, err
	}
	return evt, nil
}

func (s *Sink) seqKey() string {
	return s.Body.Public().Hex() + "\x00" + s.ThreadID
}

func (s *Sink) peekSeq() (uint64, error) {
	return processSeq.peek(s.seqKey(), s.StateDir)
}

func (s *Sink) commitSeq() (uint64, error) {
	return processSeq.commit(s.seqKey(), s.StateDir)
}

func (s *Sink) push(evt nostr.Event) error {
	n, err := frameLen(evt)
	if err != nil {
		return err
	}
	s.discardStale()
	s.ring = append(s.ring, queuedFrame{evt: evt, n: n})
	queueBytes.add(s.Body.Public().Hex(), n)
	s.trim()
	return nil
}

func (s *Sink) discardStale() {
	for len(s.ring) > 0 && s.now().Unix()-int64(s.ring[0].evt.CreatedAt) > int64(maxPendingAge/time.Second) {
		s.dropFront()
	}
}

func (s *Sink) trim() {
	body := s.Body.Public().Hex()
	for len(s.ring) > 0 && (len(s.ring) > ringCap || queueBytes.over(body)) {
		s.dropFront()
	}
}

func (s *Sink) dropFront() {
	item := s.ring[0]
	s.ring = s.ring[1:]
	queueBytes.add(s.Body.Public().Hex(), -item.n)
	s.dropped++
}

func (s *Sink) flush(ctx context.Context) error {
	s.discardStale()
	body := s.Body.Public().Hex()
	for len(s.ring) > 0 {
		if !processLimit.allow(body, s.now()) {
			return nil
		}
		item := s.ring[0]
		s.ring = s.ring[1:]
		queueBytes.add(body, -item.n)
		if err := s.Publish(ctx, item.evt); err != nil {
			return err
		}
		processLimit.commit(body, s.now())
	}
	return nil
}

func (s *Sink) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func frameLen(evt nostr.Event) (int, error) {
	raw, err := (nostr.EventEnvelope{Event: evt}).MarshalJSON()
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}

func half(s string) (string, string) {
	mid := len(s) / 2
	for mid > 0 && !utf8.RuneStart(s[mid]) {
		mid--
	}
	if mid == 0 {
		mid = 1
		for mid < len(s) && !utf8.RuneStart(s[mid]) {
			mid++
		}
	}
	if mid >= len(s) {
		return "", ""
	}
	return s[:mid], s[mid:]
}

func splitRunes(s string, maxBytes int) []string {
	if maxBytes < 1 || len(s) <= maxBytes {
		return []string{s}
	}
	var out []string
	for len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			cut = 1
			for cut < len(s) && !utf8.RuneStart(s[cut]) {
				cut++
			}
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
