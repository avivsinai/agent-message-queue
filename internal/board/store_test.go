package board

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/thread"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The first reconciliation slice publishes permanent tasks and exact owner
// corrections. The old source-per-refresh model had no durable admission or
// cancellation state, so replay restored withdrawn asks and replaced identity.
func TestPublishedTasksKeepIdentityAndOwnerCorrections(t *testing.T) {
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.StableTreeIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(base, identity, "host-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	ref, _ := Bead("demo-a1b")
	ask := Message{Namespace: base, Copies: []MessageCopy{{Root: base}}, Key: base + "#ask", Root: base, ID: "ask", Thread: "feature", Subject: "Review feature", Kind: "review_request", Body: "Review the result"}
	withdrawal := Message{Namespace: base, Copies: []MessageCopy{{Root: base}}, Key: base + "#withdrawal", Root: base, ID: "withdrawal", Thread: "feature", Kind: "decision", Body: "Withdraw this review request"}
	var evidence [][]byte
	for _, target := range []*Message{&ask, &withdrawal} {
		entry := thread.Entry{ID: target.ID, Thread: target.Thread, Subject: target.Subject, Kind: target.Kind, Body: target.Body}
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		target.EvidenceHash = hex.EncodeToString(sum[:])
		evidence = append(evidence, raw)
	}
	acquisition := Acquisition{Evidence: evidence, Input: Input{Dir: base, Known: map[string]string{"demo-a1b": "in_progress"}, Results: []NamedResult{{Name: "beads", Result: Result{Status: StatusOK, ObservedAt: now, Items: []Item{{Ref: ref, Title: "Deliver feature", State: StateActive, Owner: Owner{Kind: OwnerAgent, Handle: "worker"}, Structural: true}}}}}}, BeadsStore: filepath.Join(base, ".beads"), Messages: []Message{ask, withdrawal}}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	first, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tasks) != 1 {
		t.Fatalf("native task count = %d", len(first.Tasks))
	}
	var nativeID string
	for id := range first.Tasks {
		nativeID = id
	}
	model := first.Project(DefaultConfig(), now, "test")
	if len(model.Unresolved) != 1 {
		t.Fatalf("unadmitted ask count = %d", len(model.Unresolved))
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "attach", TaskID: nativeID, MessageKey: ask.Key}); err != nil {
		t.Fatal(err)
	}
	// Wave1 review: one message may support independent outcomes. Admission
	// anchors, rather than attachment membership, own permanent task identity.
	firstOutcome, err := store.Apply(ctx, OwnerAction{Kind: "admit", MessageKey: ask.Key, Anchor: "outcome-a", Title: "Deliver related outcome"})
	if err != nil {
		t.Fatal(err)
	}
	secondOutcome, err := store.Apply(ctx, OwnerAction{Kind: "admit", MessageKey: ask.Key, Anchor: "outcome-b", Title: "Deliver other outcome"})
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.Apply(ctx, OwnerAction{Kind: "admit", MessageKey: ask.Key, Anchor: "outcome-a", Title: "Ignored replay title"})
	if err != nil {
		t.Fatal(err)
	}
	if firstOutcome == nativeID || secondOutcome == nativeID || firstOutcome == secondOutcome || repeated != firstOutcome {
		t.Fatal("attachment membership replaced admission identity")
	}
	bound, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{nativeID, firstOutcome, secondOutcome} {
		if len(bound.Tasks[id].Attachments) != 1 {
			t.Fatal("attachment removed another task binding")
		}
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "detach", TaskID: firstOutcome, MessageKey: ask.Key}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "move", TaskID: firstOutcome, FromTaskID: secondOutcome, MessageKey: ask.Key}); err != nil {
		t.Fatal(err)
	}
	corrected, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(corrected.Tasks[nativeID].Attachments) != 1 || len(corrected.Tasks[firstOutcome].Attachments) != 1 || len(corrected.Tasks[secondOutcome].Attachments) != 0 {
		t.Fatal("scoped correction changed another binding")
	}
	for _, id := range []string{firstOutcome, secondOutcome} {
		if _, err = store.Apply(ctx, OwnerAction{Kind: "cancel", Scope: "task", TaskID: id}); err != nil {
			t.Fatal(err)
		}
	}
	// A worktree change with the same database namespace reuses native identity.
	acquisition.Input.Dir = filepath.Join(base, "another-worktree")
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Tasks) != 3 {
		t.Fatal("worktree created a second native task")
	}
	otherStore := acquisition
	otherStore.BeadsStore = filepath.Join(base, "different-database")
	if err = store.Commit(ctx, otherStore); err != nil {
		t.Fatal(err)
	}
	distinct, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(distinct.Tasks) != 4 {
		t.Fatal("distinct databases merged by bead ID")
	}
	for id, task := range distinct.Tasks {
		if id != nativeID && task.Authority != "operator" {
			if _, err = store.Apply(ctx, OwnerAction{Kind: "cancel", Scope: "task", TaskID: id}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = store.Adopt(ctx, []WithdrawalAdoption{{MessageKey: ask.Key, WithdrawalKey: withdrawal.Key}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "cancel", Scope: "task", TaskID: nativeID}); err != nil {
		t.Fatal(err)
	}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Tasks) != 4 || !replay.Tasks[nativeID].Cancelled || !replay.ActionCancelled[ask.Key] {
		t.Fatal("source replay changed identity or owner cancellations")
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "reopen", Scope: "task", TaskID: nativeID}); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	model = reopened.Project(DefaultConfig(), now, "test")
	cards := 0
	for _, column := range model.Columns {
		for _, card := range column.Cards {
			cards++
			if card.ID != nativeID || len(card.Attachments) != 1 || !card.Attachments[0].Cancelled {
				t.Fatalf("wrong unified task card: %+v", card)
			}
			if !strings.Contains(strings.Join(card.Marks, " "), "execution not observed") {
				t.Fatal("tracker progress presented as observed execution")
			}
		}
	}
	if cards != 1 || len(model.Unresolved) != 0 {
		t.Fatalf("cards=%d unresolved=%d", cards, len(model.Unresolved))
	}
	for _, render := range []func(*bytes.Buffer) error{func(b *bytes.Buffer) error { return WriteText(b, model) }, func(b *bytes.Buffer) error { return WriteHTML(b, model, "") }, func(b *bytes.Buffer) error { return json.NewEncoder(b).Encode(model) }} {
		var output bytes.Buffer
		if err = render(&output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "Deliver feature") || !strings.Contains(output.String(), "Review the result") || !strings.Contains(output.String(), nativeID) {
			t.Fatal("face omitted unified task or exact attachment")
		}
	}
	checkNativeAdmission(t, ctx)
	// Snapshot capacity reserves room for a coverage-only publication and
	// leaves all previously acknowledged corrections and IDs intact.
	oversized := acquisition
	oversized.Input.Results = append([]NamedResult{}, acquisition.Input.Results...)
	oversized.Input.Results = append(oversized.Input.Results, NamedResult{Name: "large-source", Result: Result{Detail: strings.Repeat("x", snapshotLimit-snapshotReserve)}})
	if err = store.Commit(ctx, oversized); !errors.Is(err, errSnapshotBound) {
		t.Fatalf("snapshot capacity: %v", err)
	}
	capacityState, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(capacityState.Tasks) != 4 || !capacityState.ActionCancelled[ask.Key] {
		t.Fatal("snapshot capacity lost IDs or corrections")
	}
	gap := false
	for _, source := range capacityState.Input.Results {
		if source.Name == "board-capacity" && source.Result.Coverage.Partial() {
			gap = true
		}
	}
	if !gap {
		t.Fatal("snapshot capacity kept healthy coverage")
	}
	// A live writer's pending marker is not an abandoned publication. Readers
	// wait cancellably on the lock before they classify the store.
	release, ok, err := lock.TryExclusive(filepath.Join(store.Dir, "writer.lock"))
	if err != nil || !ok {
		t.Fatalf("lock: %v", err)
	}
	if err = os.WriteFile(filepath.Join(store.Dir, "write-pending"), []byte("pending"), 0o600); err != nil {
		release()
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = store.ReadContext(waiting)
	cancel()
	release()
	if !errors.Is(err, lock.ErrStopped) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy reader returned %v", err)
	}
	if err = os.Remove(filepath.Join(store.Dir, "write-pending")); err != nil {
		t.Fatal(err)
	}
	// Wave1 review: a full evidence store must publish incomplete coverage
	// while retaining task IDs and corrections, then stop autonomous ingestion.
	full, err := os.OpenFile(filepath.Join(store.Dir, "evidence", "capacity"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err = full.Truncate(evidenceLimit); err != nil {
		_ = full.Close()
		t.Fatal(err)
	}
	if err = full.Close(); err != nil {
		t.Fatal(err)
	}
	over := acquisition
	over.Evidence = append(append([][]byte{}, acquisition.Evidence...), []byte(`{"id":"new"}`))
	if err = store.Commit(ctx, over); !errors.Is(err, errEvidenceBound) {
		t.Fatalf("full evidence store: %v", err)
	}
	bounded, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Tasks) != 4 || !bounded.ActionCancelled[ask.Key] {
		t.Fatal("evidence bound lost identities or correction")
	}
	foundGap := false
	for _, result := range bounded.Input.Results {
		if result.Name == "amq-history" && result.Result.Status == StatusDegraded && result.Result.Coverage.Partial() {
			foundGap = true
		}
	}
	if !foundGap {
		t.Fatal("evidence bound kept healthy coverage")
	}
	// Inline body text cannot hide missing or corrupted immutable evidence.
	evidencePath := filepath.Join(store.Dir, "evidence", ask.EvidenceHash+".json")
	original, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(evidencePath); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(); err == nil {
		t.Fatal("missing evidence accepted")
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "reopen", Scope: "task", TaskID: nativeID}); err == nil {
		t.Fatal("owner correction accepted unverified evidence")
	}
	if err = os.WriteFile(evidencePath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(); err == nil {
		t.Fatal("corrupt evidence accepted")
	}
	if err = os.WriteFile(evidencePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(); err != nil {
		t.Fatal(err)
	}
}

// The ratified design review observed that cache loss would allocate fresh
// task IDs. An initialized store with missing state must require recovery.
func TestMissingPublishedStateRequiresRecovery(t *testing.T) {
	base := t.TempDir()
	identity, err := fsq.StableTreeIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(base, identity, "host-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(store.Dir, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(); err == nil {
		t.Fatal("missing published state accepted")
	}
	if err = store.Init(context.Background()); err == nil {
		t.Fatal("initialized store silently reset")
	}
}

// Native-wave review: a grant removed during acquisition cannot admit tasks;
// goal replacement and identical task IDs in separate lists keep identities.
func checkNativeAdmission(t *testing.T, ctx context.Context) {
	t.Helper()
	base := t.TempDir()
	identity, err := fsq.StableTreeIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(base, identity, "host-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	key, err := store.BindClaudeList(ctx, home, "team", false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	grant := before.ClaudeLists[key]
	item := Item{Ref: Ref{Kind: "claude-task", ID: "1"}, Namespace: key, Container: "team", BindingRevision: grant.Revision, Title: "Deliver Claude task", State: StateActive, Owner: Owner{Kind: OwnerUnmapped, Display: "native-owner"}, Structural: true}
	acquisition := Acquisition{Input: Input{Results: []NamedResult{{Name: "claude-tasks", Result: Result{Status: StatusOK, Items: []Item{item}, NativeScopes: map[string]NativeScope{key: {Complete: true, BindingRevision: grant.Revision}}}}}}}
	if _, err = store.BindClaudeList(ctx, home, "team", true); err != nil {
		t.Fatal(err)
	}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	rejected, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected.Tasks) != 0 {
		t.Fatal("removed native binding admitted task")
	}
	if _, err = store.BindClaudeList(ctx, home, "team", false); err != nil {
		t.Fatal(err)
	}
	bound, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	item.BindingRevision = bound.ClaudeLists[key].Revision
	acquisition.Input.Results[0].Result.Items = []Item{item}
	acquisition.Input.Results[0].Result.NativeScopes[key] = NativeScope{Complete: true, BindingRevision: item.BindingRevision}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	goal := Item{Ref: Ref{Kind: "codex-goal", ID: "119f0000-0000-4000-8000-000000000001"}, Container: "019f0000-0000-7000-8000-000000000001", Namespace: home, Title: "Deliver Codex goal", State: StateActive, Structural: true}
	native := Acquisition{Input: Input{Results: []NamedResult{{Name: "codex-goals", Result: Result{Status: StatusOK, Items: []Item{goal}, NativeScopes: map[string]NativeScope{home: {Complete: true}}}}, {Name: "amq", Result: Result{Status: StatusOK, Items: []Item{{Ref: Ref{Kind: "bead", ID: "fake-a1"}, Namespace: home, Title: "Unadmitted request", Structural: true}}}}}}}
	if err = store.Commit(ctx, native); err != nil {
		t.Fatal(err)
	}
	first, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tasks) != 2 {
		t.Fatal("native allowlist admitted a generic structural row")
	}
	var goalID string
	for id, task := range first.Tasks {
		if task.Fact.Source == "codex-goals" {
			goalID = id
		}
	}
	goal.Ref.ID = "119f0000-0000-4000-8000-000000000002"
	native.Input.Results[0].Result.Items = []Item{goal}
	if err = store.Commit(ctx, native); err != nil {
		t.Fatal(err)
	}
	replaced, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced.Tasks) != 3 || replaced.Tasks[goalID].Cancelled || !replaced.Tasks[goalID].SourceMissing {
		t.Fatal("goal replacement changed permanent identity or cancellation")
	}
	model := replaced.Project(DefaultConfig(), time.Now(), "test")
	if len(model.Historical) != 1 || model.Historical[0].ID != goalID || model.Historical[0].Items[0].State != StateActive {
		t.Fatal("uncancelled replaced goal did not retain its historical source fact")
	}
	for _, column := range model.Columns {
		for _, card := range column.Cards {
			if card.ID == goalID {
				t.Fatal("historical goal counted as current work")
			}
		}
	}
	for _, next := range model.Next {
		if next.CardID == goalID {
			t.Fatal("historical goal selected as Next")
		}
	}
	// A pending task with only owner corrections does not offer cancellation as
	// a context action or promote itself into Next.
	item.State = StateOpenUnowned
	acquisition.Input.Results[0].Result.Items = []Item{item}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	model = pending.Project(DefaultConfig(), time.Now(), "test")
	for _, column := range model.Columns {
		for _, card := range column.Cards {
			if card.Items[0].Source != "claude-tasks" {
				continue
			}
			if hasCopyAction(card) {
				t.Fatal("owner-only correction treated as useful context")
			}
			for _, next := range model.Next {
				if next.CardID == card.ID {
					t.Fatal("mutation-only pending native task selected as Next")
				}
			}
		}
	}
	var page bytes.Buffer
	if err = WriteHTML(&page, model, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), "Historical; not reported") || !strings.Contains(page.String(), "No context command available") || strings.Contains(page.String(), "Copy context command") {
		t.Fatal("native historical/context status missing from rendered face")
	}
	// A home below a symlinked parent remains revocable after it disappears.
	parent := t.TempDir()
	actual := filepath.Join(parent, "actual")
	alias := filepath.Join(parent, "alias")
	homePath := filepath.Join(alias, "missing-home")
	if err = os.MkdirAll(filepath.Join(actual, "missing-home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	storedKey, err := store.BindClaudeList(ctx, homePath, "missing", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(actual, "missing-home")); err != nil {
		t.Fatal(err)
	}
	if _, err = store.BindClaudeList(ctx, homePath, "missing", true); err != nil {
		t.Fatal(err)
	}
	revoked, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := revoked.ClaudeLists[storedKey]; ok {
		t.Fatal("canonical missing-home binding survived revocation")
	}
}
