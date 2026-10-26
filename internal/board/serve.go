package board

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

// ServeEvery is how often a live view rebuilds the model. Sources with a
// longer cadence (the forge cache TTL, a provider's TTL) answer from their
// cache between their own refreshes.
const ServeEvery = 60 * time.Second

// ManualEvery is the least time between two forced refreshes: a press of
// refresh within it of the last forced one is coalesced into that one.
const ManualEvery = 60 * time.Second

// Live is the state of `amq-board serve`: the last built model, its JSON and
// page bytes, and the configuration it was built from. At most one refresh
// runs at a time, so each source has at most one read in flight; a tick or a
// press that finds one running joins it instead of starting another.
//
// Each refresh re-reads the configuration. A valid change applies to that
// refresh. A refused edit (a warning that the last accepted load did not
// have) keeps the last good configuration and is reported once.
type Live struct {
	// Load reads the configuration, as LoadConfig does.
	Load func() (FileConfig, []string)
	// Build reads the sources the configuration enables and builds the
	// model; force ignores the forge cache and provider TTLs.
	Build func(ctx context.Context, cfg FileConfig, force bool) Model
	Now   func() time.Time
	Logf  func(format string, args ...any)
	// Ctx is the life of the view. A refresh that a press starts runs under
	// it, so its source reads and their commands end when the view ends.
	Ctx context.Context

	active    sync.WaitGroup // refreshes running
	mu        sync.Mutex
	running   bool
	lastForce time.Time
	model     Model
	modelJSON []byte
	page      []byte

	// Touched only by the one running refresh.
	started  bool
	cfg      FileConfig
	accepted map[string]bool
	reported string
}

// Refresh runs one refresh now and returns true, or returns false at once
// when one is already running.
func (l *Live) Refresh(ctx context.Context, force bool) bool {
	if !l.begin(force) {
		return false
	}
	l.run(ctx, force)
	return true
}

// Request is a press of refresh: it joins a running refresh, coalesces into
// a forced refresh less than ManualEvery old, or starts a forced refresh in
// the background. It never waits for the sources.
func (l *Live) Request() string {
	l.mu.Lock()
	switch {
	case l.running:
		l.mu.Unlock()
		return "joined"
	case !l.lastForce.IsZero() && l.Now().Sub(l.lastForce) < ManualEvery:
		l.mu.Unlock()
		return "coalesced"
	}
	l.running, l.lastForce = true, l.Now()
	l.active.Add(1)
	l.mu.Unlock()
	go l.run(l.Ctx, true)
	return "started"
}

// Wait returns when no refresh runs. After Ctx ends, a serve that waits here
// has reaped every command its refreshes started.
func (l *Live) Wait() {
	l.active.Wait()
}

// Loop refreshes every interval until ctx ends. A tick that finds a refresh
// running is skipped, not queued.
func (l *Live) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Refresh(ctx, false)
		}
	}
}

// Model is the last built model.
func (l *Live) Model() Model {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.model
}

func (l *Live) begin(force bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return false
	}
	l.running = true
	l.active.Add(1)
	if force {
		l.lastForce = l.Now()
	}
	return true
}

func (l *Live) run(ctx context.Context, force bool) {
	defer func() {
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
		l.active.Done()
	}()
	start := time.Now()
	m := l.Build(ctx, l.ReloadConfig(), force)
	if ctx.Err() != nil {
		// The view is ending; a model of cancelled reads is not kept.
		return
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		l.Logf("refresh: model not encoded: %v", err)
		return
	}
	data = append(data, '\n')
	var page bytes.Buffer
	if err := WriteLiveHTML(&page, m); err != nil {
		l.Logf("refresh: page not rendered: %v", err)
		return
	}
	l.mu.Lock()
	l.model, l.modelJSON, l.page = m, data, page.Bytes()
	l.mu.Unlock()
	how := "refresh"
	if force {
		how = "forced refresh"
	}
	l.Logf("%s took %s", how, time.Since(start).Round(time.Millisecond))
}

// ReloadConfig applies valid live edits and keeps the last accepted configuration.
// Call it from the single refresh or acquisition loop.
func (l *Live) ReloadConfig() FileConfig {
	cfg, warnings := l.Load()
	if !l.started {
		// A start has no last good value: it runs with what loaded.
		l.started, l.cfg, l.accepted = true, cfg, set(warnings)
		for _, w := range warnings {
			l.Logf("config: %s", w)
		}
		return cfg
	}
	var fresh []string
	for _, w := range warnings {
		if !l.accepted[w] {
			// A one-shot run falls back to the defaults; here the last
			// good configuration stays.
			fresh = append(fresh, strings.TrimSuffix(w, "; using defaults"))
		}
	}
	if len(fresh) > 0 {
		if msg := strings.Join(fresh, "; "); msg != l.reported {
			l.Logf("config: edit refused, keeping the last good configuration: %s", msg)
			l.reported = msg
		}
		return l.cfg
	}
	if !reflect.DeepEqual(cfg, l.cfg) {
		l.Logf("config: change applied")
	}
	l.cfg, l.accepted, l.reported = cfg, set(warnings), ""
	return cfg
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// Handler serves the live view. Every route lives under the token:
// /<token>/ (the page), /<token>/model.json (the last built model, the same
// JSON as `amq-board --json`), and /<token>/refresh (a press of refresh). A
// path without the exact token is 404, a Host other than host (the address
// the view listens on) is refused against DNS rebinding, and any method but
// GET is 405. No request runs a source read except through Request.
func (l *Live) Handler(token, host string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		if r.Host != host {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		seg, rest, slash := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if subtle.ConstantTimeCompare([]byte(seg), []byte(token)) != 1 {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			h.Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !slash {
			// The page asks for its data by relative URL, so it must sit
			// under the token's directory.
			http.Redirect(w, r, "/"+token+"/", http.StatusMovedPermanently)
			return
		}
		switch rest {
		case "":
			l.mu.Lock()
			page := l.page
			l.mu.Unlock()
			h.Set("Content-Security-Policy", liveCSP+"; frame-ancestors 'none'")
			h.Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
		case "model.json":
			l.mu.Lock()
			data := l.modelJSON
			l.mu.Unlock()
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		case "refresh":
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"refresh": l.Request()})
		default:
			http.NotFound(w, r)
		}
	})
}
