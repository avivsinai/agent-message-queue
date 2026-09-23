// Package activity projects native harness notifications into kind 24200
// telemetry frames. It does not run a relay connection. The caller publishes
// each signed event through a seam with the same shape as relay.Conn.Publish.
package activity

import (
	"context"
	"fmt"
	"sync"
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

	ringCap     = 800
	ratePerBody = 100
	seqBlock    = 1024
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

	seq   uint64
	seqHi uint64
	ring  []nostr.Event
}

// Accept maps one Codex notification and publishes the frames that fit the
// process-wide per-body rate. A notification for another thread is ignored.
func (s *Sink) Accept(ctx context.Context, n codex.Notification) error {
	if s.Publish == nil {
		return fmt.Errorf("activity publish is not set")
	}
	obs, ok := projectCodex(s.ThreadID, n)
	if !ok {
		return nil
	}
	events, err := s.frames(obs)
	if err != nil {
		return err
	}
	for _, evt := range events {
		s.push(evt)
	}
	return s.flush(ctx)
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
	probe.Seq = s.seq + 1
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
	if !processLimit.allow(s.Body.Hex(), s.now()) {
		return nil, nil
	}
	obs.Seq = s.nextSeq()
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

func (s *Sink) nextSeq() uint64 {
	if s.seq == s.seqHi {
		s.seqHi = s.seq + seqBlock
	}
	s.seq++
	return s.seq
}

func (s *Sink) push(evt nostr.Event) {
	if len(s.ring) == ringCap {
		s.ring = s.ring[1:]
	}
	s.ring = append(s.ring, evt)
}

func (s *Sink) flush(ctx context.Context) error {
	for len(s.ring) > 0 {
		evt := s.ring[0]
		s.ring = s.ring[1:]
		if err := s.Publish(ctx, evt); err != nil {
			return err
		}
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

// limiter is the process-wide cap of 100 frames in any one-second window
// per body key.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *limiter) allow(body string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	if len(kept) >= ratePerBody {
		l.hits[body] = kept
		return false
	}
	l.hits[body] = append(kept, now)
	return true
}

var processLimit limiter
