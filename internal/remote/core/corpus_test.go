package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

const corpusPath = "../../../testdata/remote/corpus.json"

type corpusFile struct {
	Defaults struct {
		TargetID    string `json:"target_id"`
		Epoch       string `json:"epoch"`
		CreatorHost string `json:"creator_host"`
		NotAfter    string `json:"not_after"`
		Now         string `json:"now"`
	} `json:"defaults"`
	Fixtures []fixture `json:"fixtures"`
}

type fixture struct {
	ID         string           `json:"id"`
	Title      string           `json:"title"`
	Boundaries []string         `json:"boundaries"`
	Steps      []map[string]any `json:"steps"`
}

// TestContractCorpus runs every fixture in testdata/remote/corpus.json against
// the endpoint and the fake runtime. Q19 runs once per crash boundary.
func TestContractCorpus(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(corpusPath))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c corpusFile
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	for _, f := range c.Fixtures {
		f := f
		if len(f.Boundaries) == 0 {
			t.Run(f.ID, func(t *testing.T) { newHarness(t, &c, "").run(f) })
			continue
		}
		for _, b := range f.Boundaries {
			b := b
			t.Run(f.ID+"/"+b, func(t *testing.T) { newHarness(t, &c, b).run(f) })
		}
	}
}

type harness struct {
	t          *testing.T
	c          *corpusFile
	dir        string
	fake       *fake.Runtime
	store      *requests.Store
	ep         *core.Endpoint
	clock      time.Time
	crashPoint string
	crashArmed bool
	initialSID string

	mu          sync.Mutex
	history     map[string][]protocol.State
	published   []protocol.Snapshot
	dropPublish bool
	pending     map[string]chan result
}

type result struct {
	reply any
	err   error
}

func newHarness(t *testing.T, c *corpusFile, boundary string) *harness {
	now, err := protocol.ParseTime(c.Defaults.Now)
	if err != nil {
		t.Fatalf("defaults.now: %v", err)
	}
	h := &harness{
		t:          t,
		c:          c,
		dir:        t.TempDir(),
		fake:       fake.New(c.Defaults.TargetID, c.Defaults.Epoch),
		clock:      now,
		crashPoint: boundary,
		crashArmed: boundary != "",
		history:    map[string][]protocol.State{},
		pending:    map[string]chan result{},
	}
	h.initialSID = h.fake.SessionID()
	h.open()
	return h
}

func (h *harness) open() {
	store, err := requests.Open(h.dir, requests.WithClock(func() time.Time { return h.clock }))
	if err != nil {
		h.t.Fatalf("open store: %v", err)
	}
	h.store = store
	h.ep = core.New(core.Config{
		Store: store,
		Now:   func() time.Time { return h.clock },
		Publish: func(s protocol.Snapshot, _ map[string]string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.dropPublish {
				h.dropPublish = false
				return errors.New("publication dropped")
			}
			h.published = append(h.published, s)
			return nil
		},
		Crash: func(point string) error {
			if h.crashArmed && point == h.crashPoint {
				h.crashArmed = false
				return errors.New("simulated crash")
			}
			return nil
		},
	})
	h.ep.Observe(func(rec *requests.Record) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.history[rec.RequestID] = append(h.history[rec.RequestID], rec.State)
	})
	h.ep.Register(h.fake)
	if err := h.ep.Reconcile(); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

func (h *harness) run(f fixture) {
	for i, step := range f.Steps {
		h.step(fmt.Sprintf("%s step %d", f.ID, i), step)
	}
	if err := h.ep.Close(); err != nil {
		h.t.Fatalf("close: %v", err)
	}
}

func (h *harness) step(where string, step map[string]any) {
	switch {
	case step["client"] != nil:
		n := 1
		if r, ok := step["repeat"].(float64); ok {
			n = int(r)
		}
		for i := 0; i < n; i++ {
			h.client(where, step)
		}
	case step["native"] != nil:
		h.native(where, step)
	case step["expect"] != nil:
		h.expect(where, step["expect"].(map[string]any))
	case step["expect_native"] != nil:
		h.expectNative(where, step["expect_native"].(map[string]any))
	case step["expect_published"] != nil:
		h.expectPublished(where, step["expect_published"].(map[string]any))
	case step["cli"] != nil:
		h.cli(where, step)
	case step["endpoint"] != nil:
		h.endpointControl(where, step)
	case step["clock"] != nil:
		now, err := protocol.ParseTime(step["clock"].(string))
		if err != nil {
			h.t.Fatalf("%s: clock: %v", where, err)
		}
		h.clock = now
	case step["restart"] != nil:
		if err := h.ep.Close(); err != nil {
			h.t.Fatalf("%s: close: %v", where, err)
		}
		h.open()
	default:
		h.t.Fatalf("%s: unknown step %v", where, step)
	}
}

func (h *harness) command(raw map[string]any) (*protocol.Command, core.Source) {
	cmd := map[string]any{"schema": protocol.SchemaCommand}
	for k, v := range raw {
		cmd[k] = v
	}
	host := h.c.Defaults.CreatorHost
	if s, ok := cmd["creator_host"].(string); ok {
		host = s
		delete(cmd, "creator_host")
	}
	if id, ok := cmd["request_ref_for"].(string); ok {
		cmd["request_ref"] = protocol.EncodeRef(h.c.Defaults.CreatorHost, h.c.Defaults.TargetID, id)
		delete(cmd, "request_ref_for")
	}
	op := protocol.Op(cmd["op"].(string))
	switch op {
	case protocol.OpRequestSubmit, protocol.OpRequestCancel, protocol.OpInteractionRespond:
		setDefault(cmd, "target_id", h.c.Defaults.TargetID)
		setDefault(cmd, "epoch", h.c.Defaults.Epoch)
		if op != protocol.OpInteractionRespond {
			setDefault(cmd, "not_after", h.c.Defaults.NotAfter)
		}
	case protocol.OpSessionInspect, protocol.OpSessionEvents:
		setDefault(cmd, "target_id", h.c.Defaults.TargetID)
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		h.t.Fatalf("marshal command: %v", err)
	}
	decoded, err := protocol.DecodeCommand(data)
	if err != nil {
		h.t.Fatalf("decode command %s: %v", data, err)
	}
	return decoded, core.Source{Host: host}
}

func setDefault(m map[string]any, key, value string) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}

func (h *harness) client(where string, step map[string]any) {
	cmd, src := h.command(step["client"].(map[string]any))
	if step["async"] == true {
		ch := make(chan result, 1)
		h.mu.Lock()
		h.pending[cmd.RequestID] = ch
		h.mu.Unlock()
		go func() {
			reply, err := h.ep.Handle(cmd, src)
			ch <- result{reply, err}
		}()
		time.Sleep(20 * time.Millisecond) // let the submit reach the held admission gate
		return
	}
	reply, err := h.ep.Handle(cmd, src)
	if step["may_crash"] == true && errors.Is(err, core.ErrCrashed) {
		return
	}
	if step["deferred"] == true {
		if err != nil {
			h.t.Fatalf("%s: deferred submit errored: %v", where, err)
		}
		return
	}
	expected, _ := step["reply"].(map[string]any)
	if expected == nil {
		if err != nil {
			h.t.Fatalf("%s: unexpected error: %v", where, err)
		}
		return
	}
	got := toMap(h.t, reply)
	if err != nil {
		var r *protocol.Refusal
		if !errors.As(err, &r) {
			h.t.Fatalf("%s: unexpected error: %v", where, err)
		}
		got = map[string]any{"code": string(r.Code)}
	}
	assertSubset(h.t, where, expected, got)
}

func toMap(t *testing.T, v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		// list replies
		return map[string]any{"list": string(data)}
	}
	return m
}

func assertSubset(t *testing.T, where string, expected, got map[string]any) {
	for k, ev := range expected {
		gv, ok := got[k]
		if !ok {
			t.Fatalf("%s: reply lacks %q; got %v", where, k, got)
		}
		if em, isMap := ev.(map[string]any); isMap {
			gm, _ := gv.(map[string]any)
			if gm == nil {
				t.Fatalf("%s: reply %q is not an object: %v", where, k, gv)
			}
			assertSubset(t, where+"."+k, em, gm)
			continue
		}
		if fmt.Sprint(ev) != fmt.Sprint(gv) {
			t.Fatalf("%s: reply %q = %v, want %v (reply %v)", where, k, gv, ev, got)
		}
	}
}

func (h *harness) native(where string, step map[string]any) {
	action := step["native"].(string)
	id, _ := step["request_id"].(string)
	text, _ := step["text"].(string)
	switch action {
	case "hold_admission":
		h.fake.HoldAdmission()
	case "release_admission":
		h.fake.ReleaseAdmission()
		h.mu.Lock()
		ch := h.pending[id]
		if ch == nil {
			for _, c := range h.pending {
				ch = c
			}
		}
		h.mu.Unlock()
		if ch != nil {
			select {
			case r := <-ch:
				if r.err != nil {
					h.t.Fatalf("%s: async submit errored: %v", where, r.err)
				}
			case <-time.After(2 * time.Second):
				h.t.Fatalf("%s: async submit did not return", where)
			}
		}
	case "complete":
		if !h.fake.Complete(id, text) {
			h.t.Fatalf("%s: no running run for %s", where, id)
		}
	case "complete_if_admitted":
		h.fake.Complete(id, text)
	case "idle", "idle_blip":
	case "local_draft":
		h.fake.LocalDraft(text)
	case "local_input":
		h.fake.LocalInput(text)
	case "admission_fails_after_return":
		h.fake.FailNextAdmissionAfterReturn()
	case "switch_session":
		h.fake.SwitchSession(step["new_epoch"].(string))
	case "question":
		var opts []string
		for _, o := range step["options"].([]any) {
			opts = append(opts, o.(string))
		}
		h.fake.Question(id, step["interaction_id"].(string), opts)
	case "local_answer":
		h.fake.LocalAnswer(step["interaction_id"].(string), step["option"].(string))
	case "offline":
		h.fake.SetOffline(true)
	case "online":
		h.fake.SetOffline(false)
		if err := h.ep.Tick(); err != nil {
			h.t.Fatalf("%s: tick: %v", where, err)
		}
	default:
		h.t.Fatalf("%s: unknown native action %q", where, action)
	}
}

func (h *harness) record(id string) (*requests.Record, bool) {
	rec, ok, err := h.store.Get(requests.Key{CreatorHost: h.c.Defaults.CreatorHost, TargetID: h.c.Defaults.TargetID, RequestID: id})
	if err != nil {
		h.t.Fatalf("get record: %v", err)
	}
	return rec, ok
}

func (h *harness) expect(where string, exp map[string]any) {
	id, _ := exp["request_id"].(string)
	var rec *requests.Record
	if id != "" {
		var ok bool
		rec, ok = h.record(id)
		if !ok {
			rec = &requests.Record{}
			rec.State = "absent"
		}
	}
	for k, v := range exp {
		switch k {
		case "request_id":
		case "native_dispatches":
			if got := h.fake.Snapshot().Dispatches; got != int(v.(float64)) {
				h.t.Fatalf("%s: native dispatches %d, want %v", where, got, v)
			}
		case "native_dispatches_max":
			if got := h.fake.Snapshot().Dispatches; got > int(v.(float64)) {
				h.t.Fatalf("%s: native dispatches %d, want at most %v", where, got, v)
			}
		case "native_aborts":
			if got := h.fake.Snapshot().Aborts; got != int(v.(float64)) {
				h.t.Fatalf("%s: native aborts %d, want %v", where, got, v)
			}
		case "input_text":
			if rec.Input == nil || rec.Input.Text != v.(string) {
				h.t.Fatalf("%s: input text %v, want %v", where, rec.Input, v)
			}
		case "state":
			if string(rec.State) != v.(string) {
				h.t.Fatalf("%s: state %s, want %v", where, rec.State, v)
			}
		case "state_in":
			if !contains(v.([]any), string(rec.State)) {
				h.t.Fatalf("%s: state %s not in %v", where, rec.State, v)
			}
		case "never_state":
			h.mu.Lock()
			hist := h.history[id]
			h.mu.Unlock()
			for _, s := range hist {
				if contains(v.([]any), string(s)) {
					h.t.Fatalf("%s: state %s appeared in history %v", where, s, hist)
				}
			}
		case "no_orphan_run":
			for _, k := range h.fake.Snapshot().RunningKeys {
				if k.RequestID == id && rec.State != protocol.StateRunning {
					h.t.Fatalf("%s: fake has a running run for %s while record is %s", where, id, rec.State)
				}
			}
		case "creator_host":
			if rec.CreatorHost != v.(string) {
				h.t.Fatalf("%s: creator host %s, want %v", where, rec.CreatorHost, v)
			}
		case "records":
			all, err := h.store.List()
			if err != nil {
				h.t.Fatalf("%s: list: %v", where, err)
			}
			n := 0
			for _, r := range all {
				if r.RequestID == id {
					n++
				}
			}
			if n != int(v.(float64)) {
				h.t.Fatalf("%s: %d records for %s, want %v", where, n, id, v)
			}
		default:
			h.t.Fatalf("%s: unknown expectation %q", where, k)
		}
	}
}

func contains(list []any, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (h *harness) expectNative(where string, exp map[string]any) {
	snap := h.fake.Snapshot()
	for k, v := range exp {
		switch k {
		case "draft":
			if h.fake.Draft() != v.(string) {
				h.t.Fatalf("%s: draft %q, want %q", where, h.fake.Draft(), v)
			}
		case "session_id_unchanged":
			if (h.fake.SessionID() == h.initialSID) != v.(bool) {
				h.t.Fatalf("%s: session id changed", where)
			}
		case "consumer_sets":
			if snap.ConsumerSets != int(v.(float64)) {
				h.t.Fatalf("%s: consumer sets %d, want %v", where, snap.ConsumerSets, v)
			}
		case "answers":
			want := v.([]any)
			if len(snap.Answers) != len(want) {
				h.t.Fatalf("%s: answers %v, want %v", where, snap.Answers, want)
			}
			for i, w := range want {
				wm := w.(map[string]any)
				if snap.Answers[i].InteractionID != wm["interaction_id"] || snap.Answers[i].Option != wm["option"] {
					h.t.Fatalf("%s: answer %d = %v, want %v", where, i, snap.Answers[i], wm)
				}
			}
		default:
			h.t.Fatalf("%s: unknown native expectation %q", where, k)
		}
	}
}

func (h *harness) expectPublished(where string, exp map[string]any) {
	id := exp["request_id"].(string)
	state := exp["terminal_state"].(string)
	want := int(exp["terminal_count"].(float64))
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.published {
		if s.RequestID == id && string(s.State) == state {
			n++
		}
	}
	if n != want {
		h.t.Fatalf("%s: %d published %s snapshots for %s, want %d", where, n, state, id, want)
	}
}

func (h *harness) cli(where string, step map[string]any) {
	if step["cli"] != "wait" {
		h.t.Fatalf("%s: unknown cli step %v", where, step["cli"])
	}
	ref := protocol.EncodeRef(h.c.Defaults.CreatorHost, h.c.Defaults.TargetID, step["request_id"].(string))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	snap, err := h.ep.Wait(ctx, ref)
	if !errors.Is(err, context.DeadlineExceeded) {
		h.t.Fatalf("%s: wait returned %v / %v, want deadline", where, snap.State, err)
	}
}

func (h *harness) endpointControl(where string, step map[string]any) {
	switch step["endpoint"] {
	case "compact_results":
		if _, err := h.store.Compact(h.clock.Add(time.Second)); err != nil {
			h.t.Fatalf("%s: compact: %v", where, err)
		}
	case "storage_fail_next_write":
		dir := filepath.Join(h.store.Dir(), "requests", h.c.Defaults.CreatorHost)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			h.t.Fatalf("%s: mkdir: %v", where, err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			h.t.Fatalf("%s: chmod: %v", where, err)
		}
		h.t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	case "drop_next_publication":
		h.mu.Lock()
		h.dropPublish = true
		h.mu.Unlock()
	case "start_second":
		_, err := requests.Open(h.dir)
		var r *protocol.Refusal
		if !errors.As(err, &r) || r.Code != protocol.Code(step["expect_code"].(string)) || protocol.ExitCode(err) != int(step["expect_exit"].(float64)) {
			h.t.Fatalf("%s: second endpoint: %v", where, err)
		}
	default:
		h.t.Fatalf("%s: unknown endpoint control %v", where, step["endpoint"])
	}
}
