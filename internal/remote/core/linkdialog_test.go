package core_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// stubDialogs plays the machine's link-dialog seam: a Select the test sets
// per call, and an Outcome map. It records what it was asked, so a test can
// see which seam the endpoint consulted.
type stubDialogs struct {
	mu         sync.Mutex
	selects    int
	lastSel    func(rec *requests.Record) *protocol.Interaction
	out        map[string]protocol.Resolution
	askOut     int
	gone       map[string]bool                // ids whose files are gone: Present reports not present
	presentRes map[string]protocol.Resolution // resolutions Present sees in the outcome-read race
	policy     func(string) (bool, string)    // AnswerPolicy; nil = available
	write      func() (protocol.Code, string) // WriteAnswer; nil = durable
	answers    []string                       // WriteAnswer calls, "binding/id/label"
}

func (s *stubDialogs) AnswerPolicy(host, binding string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policy != nil {
		return s.policy(host + "/" + binding)
	}
	return true, ""
}

func (s *stubDialogs) RequestLive(*requests.Record) bool { return true }

func (s *stubDialogs) WriteAnswer(host, binding, id, label, _, _ string) (protocol.Code, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = append(s.answers, binding+"/"+id+"/"+label)
	if s.write != nil {
		return s.write()
	}
	return "", ""
}

// Present answers for a published dialog at the retirement horizon: files
// exist and no resolution replaced them, unless the test marked the id gone
// or planted a resolution in the Outcome-to-Present window.
func (s *stubDialogs) Present(_, id string) (bool, protocol.Resolution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.presentRes[id]; ok {
		return false, r, true
	}
	return !s.gone[id], protocol.Resolution{}, false
}

func (s *stubDialogs) markGone(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone == nil {
		s.gone = map[string]bool{}
	}
	s.gone[id] = true
}

func (s *stubDialogs) Select(rec *requests.Record) *protocol.Interaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selects++
	if s.lastSel == nil {
		return nil
	}
	return s.lastSel(rec)
}

func (s *stubDialogs) Outcome(_, id string) (protocol.Resolution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.askOut++
	r, ok := s.out[id]
	return r, ok
}

func (s *stubDialogs) resolve(id string, r protocol.Resolution) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		s.out = map[string]protocol.Resolution{}
	}
	s.out[id] = r
}

func (s *stubDialogs) setSelect(fn func(rec *requests.Record) *protocol.Interaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSel = fn
}

func linkInteraction(id string) *protocol.Interaction {
	return &protocol.Interaction{
		InteractionID: id, Kind: "approval", Prompt: "Bash\nls",
		Options:       []string{"Allow", "Deny"},
		ApproveOption: "Allow", RejectOption: "Deny",
	}
}

// R3: a running link request publishes the machine's selection as its
// interaction, keeps the pin while the selection is empty, and records the
// published id as a link dialog.
func TestLinkDialogSelectionPublishesAndPins(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return nil, nil })
	stub := &stubDialogs{}
	ep.SetLinkDialogs(stub)

	id := "11111111-1111-4111-8111-1111111111d1"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-1")); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(func(*requests.Record) *protocol.Interaction { return linkInteraction("ld_1") })
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("record after reconcile: ok=%v err=%v", ok, err)
	}
	if rec.Interaction == nil || rec.Interaction.InteractionID != "ld_1" {
		t.Fatalf("interaction = %+v; want the published link dialog", rec.Interaction)
	}
	if rec.Interaction.ApproveOption != "Allow" {
		t.Fatalf("approve option = %q; want Allow", rec.Interaction.ApproveOption)
	}
	if len(rec.LinkDialogs) != 1 || rec.LinkDialogs[0] != "ld_1" {
		t.Fatalf("link dialogs = %v; want [ld_1]", rec.LinkDialogs)
	}

	// An empty selection keeps the pin: the dialog stays the interaction.
	stub.setSelect(nil)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ = store.Get(key); rec.Interaction == nil || rec.Interaction.InteractionID != "ld_1" {
		t.Fatalf("empty selection dropped the pin: %+v", rec.Interaction)
	}
}

// Regression (review of c2236f5e): a pinned dialog whose record went
// uncertain after attachment loss still settles from its files/history —
// the disposition lives on the machine, not in the attachment. New
// selections stay a running record's business.
func TestLinkDialogPinnedSettlesWhileUncertain(t *testing.T) {
	store, now := openStore(t)
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(fake.New("fake", "e_1"))
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return []string{}, nil })
	stub := &stubDialogs{out: map[string]protocol.Resolution{}}
	ep.SetLinkDialogs(stub)
	id := "11111111-1111-4111-8111-1111111111d6"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-1")); err != nil {
		t.Fatal(err)
	}
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}
	stub.setSelect(func(*requests.Record) *protocol.Interaction { return linkInteraction("ld_6") })
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.Get(key)
	if rec.Interaction == nil {
		t.Fatal("fixture never published")
	}
	// Restore the endpoint with its durable record while the native target
	// is absent: the next reconcile finds the record uncertain.
	ep2 := core.New(core.Config{Store: store, Now: now})
	ep2.SetLinkDialogs(stub)
	stub.setSelect(nil)
	if err := ep2.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	if rec.State != protocol.StateUncertain {
		t.Fatalf("state %v", rec.State)
	}
	stub.resolve("ld_6", protocol.Resolution{InteractionID: "ld_6", Outcome: protocol.ResolutionElsewhere, Option: "Deny"})
	if err := ep2.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	if rec.Interaction != nil || len(rec.Resolved) != 1 {
		t.Fatalf("state=%s stillPinned=%t resolved=%v; want cleared pin and answered_elsewhere/Deny", rec.State, rec.Interaction != nil, rec.Resolved)
	}
	if rec.Resolved[0].Outcome != protocol.ResolutionElsewhere || rec.Resolved[0].Option != "Deny" {
		t.Fatalf("resolution = %+v; want elsewhere/Deny", rec.Resolved[0])
	}
}

// Ruling C2 (2026-11-23): at the retirement horizon an open dialog whose
// files still exist stays owed — the hook can wait far beyond the horizon —
// and a resolution found later gives the real outcome. Unknown is only for
// a dialog with no files and no history entry.
func TestLinkDialogOpenDialogNotRetiredAtHorizon(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return nil, nil })
	stub := &stubDialogs{}
	ep.SetLinkDialogs(stub)

	id := "11111111-1111-4111-8111-1111111111d5"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-1")); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(func(*requests.Record) *protocol.Interaction { return linkInteraction("ld_3") })
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(nil)
	rt.Complete(id, "done") // the run ends, the dialog stays open
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}

	// The horizon passes while the dialog's files exist: still owed, no
	// unknown resolution recorded.
	clk = clk.Add(protocol.DefaultCompactHorizon + time.Minute)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.Get(key)
	if !slices.Contains(rec.OwedOutcomes, "ld_3") {
		t.Fatalf("owed outcomes = %v; want ld_3 still owed", rec.OwedOutcomes)
	}
	for i := range rec.Resolved {
		if rec.Resolved[i].InteractionID == "ld_3" {
			t.Fatalf("open dialog retired at the horizon: %+v", rec.Resolved[i])
		}
	}

	// The dialog resolves late: the real outcome lands.
	stub.resolve("ld_3", protocol.Resolution{InteractionID: "ld_3", Outcome: protocol.ResolutionAnswered, Option: "Deny"})
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	var got *protocol.Resolution
	for i := range rec.Resolved {
		if rec.Resolved[i].InteractionID == "ld_3" {
			got = &rec.Resolved[i]
		}
	}
	if got == nil || got.Outcome != protocol.ResolutionAnswered || got.Option != "Deny" {
		t.Fatalf("late resolution = %+v; want answered/Deny", got)
	}
	if slices.Contains(rec.OwedOutcomes, "ld_3") {
		t.Fatalf("owed outcomes = %v; want ld_3 settled", rec.OwedOutcomes)
	}
}

// Regression (review of 9090b1e7): a resolution that lands after the
// outcome ask misses but before the horizon's present-read — the lookup
// returns it resolved — is settled, never retired as unknown.
func TestLinkDialogResolutionSeenByHorizonPresentSettles(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return nil, nil })
	stub := &stubDialogs{}
	ep.SetLinkDialogs(stub)

	id := "11111111-1111-4111-8111-1111111111d5"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-1")); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(func(*requests.Record) *protocol.Interaction { return linkInteraction("ld_3") })
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(nil)
	rt.Complete(id, "done")
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}

	// The outcome ask misses, then the resolution lands before Present
	// reads: the horizon settles it instead of publishing unknown.
	clk = clk.Add(protocol.DefaultCompactHorizon + time.Minute)
	stub.presentRes = map[string]protocol.Resolution{
		"ld_3": {InteractionID: "ld_3", Outcome: protocol.ResolutionAnswered, Option: "Deny"},
	}
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.Get(key)
	var got *protocol.Resolution
	for i := range rec.Resolved {
		if rec.Resolved[i].InteractionID == "ld_3" {
			got = &rec.Resolved[i]
		}
	}
	if got == nil || got.Outcome != protocol.ResolutionAnswered || got.Option != "Deny" {
		t.Fatalf("resolution = %+v; want answered/Deny, not unknown", got)
	}
	for i := range rec.Resolved {
		if rec.Resolved[i].InteractionID == "ld_3" && rec.Resolved[i].Outcome == protocol.ResolutionUnknown {
			t.Fatal("unknown published although the final read saw the resolution")
		}
	}
	if slices.Contains(rec.OwedOutcomes, "ld_3") || slices.Contains(rec.RetiredOutcomes, "ld_3") {
		t.Fatalf("obligations = owed %v retired %v; want ld_3 settled", rec.OwedOutcomes, rec.RetiredOutcomes)
	}
}

// R3: two running link requests of one binding never publish one dialog;
// the first pin wins and the second record publishes nothing.
func TestLinkDialogPinnedByOtherIsNotRepublished(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return nil, nil })
	stub := &stubDialogs{}
	ep.SetLinkDialogs(stub)

	ids := []string{"11111111-1111-4111-8111-1111111111d2", "11111111-1111-4111-8111-1111111111d3"}
	for _, id := range ids {
		if _, err := ep.Handle(submitCmd(id), linkShare("sess-1")); err != nil {
			t.Fatal(err)
		}
	}
	stub.setSelect(func(*requests.Record) *protocol.Interaction { return linkInteraction("ld_x") })
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	pinned := 0
	for _, id := range ids {
		rec, _, _ := store.Get(requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id})
		if rec.Interaction != nil {
			pinned++
			if rec.Interaction.InteractionID != "ld_x" {
				t.Fatalf("pinned %q; want ld_x", rec.Interaction.InteractionID)
			}
		}
	}
	if pinned != 1 {
		t.Fatalf("%d records pinned the same dialog; want one", pinned)
	}
}

// R4: the dialog's resolution clears the pinned interaction and records the
// exact outcome; the link's own answer marks the request resolved remotely.
func TestLinkDialogResolutionClearsAndRecords(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return nil, nil })
	stub := &stubDialogs{out: map[string]protocol.Resolution{}}
	ep.SetLinkDialogs(stub)

	id := "11111111-1111-4111-8111-1111111111d4"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-1")); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(func(*requests.Record) *protocol.Interaction { return linkInteraction("ld_2") })
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	stub.resolve("ld_2", protocol.Resolution{InteractionID: "ld_2", Outcome: protocol.ResolutionAnswered, Option: "Allow"})
	stub.setSelect(nil)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(key)
	if rec.Interaction != nil {
		t.Fatalf("resolved dialog still pinned: %+v", rec.Interaction)
	}
	var got *protocol.Resolution
	for i := range rec.Resolved {
		if rec.Resolved[i].InteractionID == "ld_2" {
			got = &rec.Resolved[i]
		}
	}
	if got == nil || got.Outcome != protocol.ResolutionAnswered || got.Option != "Allow" {
		t.Fatalf("resolution = %+v; want answered/Allow", got)
	}
	if len(rec.OwedOutcomes) != 0 {
		t.Fatalf("owed outcomes = %v; want none", rec.OwedOutcomes)
	}
}

// R2: a link dialog open at the run's end owes its outcome whatever the
// attachment implements (Claude and Codex implement no InteractionResolver,
// so the plain runtime is the keeper); past the retention horizon it
// retires unknown, never delivery_unknown — the files were gone, not a
// delivery in flight.
func TestLinkDialogRunEndRetiresUnknown(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return nil, nil })
	stub := &stubDialogs{}
	ep.SetLinkDialogs(stub)

	id := "11111111-1111-4111-8111-1111111111d5"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-1")); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(func(*requests.Record) *protocol.Interaction { return linkInteraction("ld_3") })
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	stub.setSelect(nil)
	stub.markGone("ld_3") // the run ends with the dialog's files already gone
	rt.Complete(id, "done")
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(key)
	if len(rec.OwedOutcomes) != 1 || rec.OwedOutcomes[0] != "ld_3" {
		t.Fatalf("owed outcomes = %v; want [ld_3]", rec.OwedOutcomes)
	}

	// The horizon passes with no outcome found anywhere: unknown.
	clk = clk.Add(protocol.DefaultCompactHorizon + time.Minute)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	var got *protocol.Resolution
	for i := range rec.Resolved {
		if rec.Resolved[i].InteractionID == "ld_3" {
			got = &rec.Resolved[i]
		}
	}
	if got == nil || got.Outcome != protocol.ResolutionUnknown {
		t.Fatalf("retained resolution = %+v; want unknown", got)
	}

	// A retired outcome stays correctable: a resolution found later
	// replaces the unknown.
	stub.resolve("ld_3", protocol.Resolution{InteractionID: "ld_3", Outcome: protocol.ResolutionAnswered, Option: "Deny"})
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	for i := range rec.Resolved {
		if rec.Resolved[i].InteractionID == "ld_3" {
			if rec.Resolved[i].Outcome != protocol.ResolutionAnswered || rec.Resolved[i].Option != "Deny" {
				t.Fatalf("late resolution = %+v; want answered/Deny", rec.Resolved[i])
			}
		}
	}
}
