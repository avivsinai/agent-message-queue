package board

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/avivsinai/agent-message-queue/internal/board/decision"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/thread"
	"os"
	"path/filepath"
	"reflect"
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
	initial, err := store.ReadContext(ctx)
	if err != nil {
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
	acquisition := Acquisition{ProjectID: initial.ProjectID, Evidence: evidence, Input: Input{Dir: base, Known: map[string]string{"demo-a1b": "in_progress"}, Results: []NamedResult{{Name: "beads", Result: Result{Status: StatusOK, ObservedAt: now, Items: []Item{{Ref: ref, Title: "Deliver feature", State: StateActive, Owner: Owner{Kind: OwnerAgent, Handle: "worker"}, Structural: true}}}}}}, BeadsStore: filepath.Join(base, ".beads"), Messages: []Message{ask, withdrawal}}
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
	// Live board: relative source ages used the old publication time.
	if !model.GeneratedAt.Equal(now) || !model.PublishedAt.Equal(first.PublishedAt) {
		t.Fatal("projection time and snapshot publication time must remain separate")
	}
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
	checkLegacyIdentityMigration(t, store, ctx)
	checkIdentityCorrections(t, ctx)
	checkSuggestionAdoption(t, ctx)
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
	// Authority review: a captured acquisition and long-lived reader must stop
	// when an explicitly initialized replacement has a different project ID.
	pinned := store
	pinned.ProjectID = initial.ProjectID
	preserved := store.Dir + "-preserved"
	if err = os.Rename(store.Dir, preserved); err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pinned.ReadContext(ctx); !errors.Is(err, ErrProjectChanged) {
		t.Fatalf("pinned reader accepted replacement project: %v", err)
	}
	if err = store.Commit(ctx, acquisition); !errors.Is(err, ErrProjectChanged) {
		t.Fatalf("old evidence acquisition accepted replacement project: %v", err)
	}
	withoutEvidence := acquisition
	withoutEvidence.Evidence = nil
	if err = store.Commit(ctx, withoutEvidence); !errors.Is(err, ErrProjectChanged) {
		t.Fatalf("old acquisition committed into replacement project: %v", err)
	}
	replacement, err := store.ReadContext(ctx)
	if err != nil || replacement.ProjectID == initial.ProjectID || replacement.Generation != 1 || len(replacement.Tasks) != 0 {
		t.Fatalf("replacement project changed after rejected acquisition: %v", err)
	}
	if _, err = os.Stat(filepath.Join(store.Dir, "evidence")); !os.IsNotExist(err) {
		t.Fatal("old acquisition staged evidence into replacement project")
	}
	preservedStore := store
	preservedStore.Dir = preserved
	prior, err := preservedStore.ReadContext(ctx)
	if err != nil || len(prior.Tasks) != 4 || !prior.ActionCancelled[ask.Key] {
		t.Fatalf("replacement lost preserved task identities or correction: %v", err)
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
	acquisition := Acquisition{ProjectID: before.ProjectID, Input: Input{Results: []NamedResult{{Name: "claude-tasks", Result: Result{Status: StatusOK, Items: []Item{item}, NativeScopes: map[string]NativeScope{key: {Complete: true, BindingRevision: grant.Revision}}}}}}}
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
	native := Acquisition{ProjectID: before.ProjectID, Input: Input{Results: []NamedResult{{Name: "codex-goals", Result: Result{Status: StatusOK, Items: []Item{goal}, NativeScopes: map[string]NativeScope{home: {Complete: true}}}}, {Name: "amq", Result: Result{Status: StatusOK, Items: []Item{{Ref: Ref{Kind: "bead", ID: "fake-a1"}, Namespace: home, Title: "Unadmitted request", Structural: true}}}}}}}
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
	if !strings.Contains(page.String(), "source no longer reports this task") || !strings.Contains(page.String(), "No context command available") || strings.Contains(page.String(), "Copy context command") {
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

// The authority slice migrates the published v1 shape without replacing any
// identity, cancellation, action disposition, attachment or native-list grant.
func checkLegacyIdentityMigration(t *testing.T, store Store, ctx context.Context) {
	t.Helper()
	if _, err := store.BindClaudeList(ctx, t.TempDir(), "migration-list", false); err != nil {
		t.Fatal(err)
	}
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	data, err := snapshotBytes(&before)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err = json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["schema"] = json.RawMessage(`"amq/board-state/v1"`)
	delete(document, "source_facts")
	var tasks map[string]map[string]json.RawMessage
	if err = json.Unmarshal(document["tasks"], &tasks); err != nil {
		t.Fatal(err)
	}
	for id, fields := range tasks {
		task := before.Tasks[id]
		fields["source_fact"], _ = json.Marshal(task.Fact)
		fields["source_observed_at"], _ = json.Marshal(task.ObservedAt)
		fields["source_missing"], _ = json.Marshal(task.SourceMissing)
		delete(fields, "owner_fact")
		delete(fields, "admission_anchors")
	}
	document["tasks"], _ = json.Marshal(tasks)
	legacy, _ := json.Marshal(document)
	if _, err = fsq.WriteFileAtomic(store.Dir, "snapshot.json", legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Schema != SnapshotSchema || migrated.Generation != before.Generation || migrated.ProjectID != before.ProjectID || migrated.HostID != before.HostID || !reflect.DeepEqual(migrated.Tasks, before.Tasks) || !reflect.DeepEqual(migrated.Anchors, before.Anchors) || !reflect.DeepEqual(migrated.ActionCancelled, before.ActionCancelled) || !reflect.DeepEqual(migrated.Withdrawals, before.Withdrawals) || !reflect.DeepEqual(migrated.ClaudeLists, before.ClaudeLists) {
		t.Fatal("v1 migration changed durable identity or owner decisions")
	}
	for id, task := range migrated.Tasks {
		kind := "reopen"
		if task.Cancelled {
			kind = "cancel"
		}
		if _, err = store.Apply(ctx, OwnerAction{Kind: kind, Scope: "task", TaskID: id}); err != nil {
			t.Fatal(err)
		}
		break
	}
	upgraded, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Schema != SnapshotSchema || len(upgraded.Tasks) != len(before.Tasks) {
		t.Fatal("migration publication replaced tasks")
	}
}

// Review found that task-owned source facts made authority depend on source
// order. Explicit consolidation/movement must preserve dispositions and replay.
func checkIdentityCorrections(t *testing.T, ctx context.Context) {
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
	project, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beadRef, _ := Bead("demo-authority")
	home := t.TempDir()
	bead := Item{Ref: beadRef, Title: "Bead outcome", State: StateActive, Structural: true}
	goal := Item{Namespace: home, Ref: Ref{Kind: "codex-goal", ID: "119f0000-0000-4000-8000-000000000001"}, Container: "019f0000-0000-7000-8000-000000000001", Title: "Chosen outcome", State: StateDone, Structural: true}
	acquisition := Acquisition{ProjectID: project.ProjectID, BeadsStore: home, Input: Input{Known: map[string]string{beadRef.ID: "in_progress"}, Results: []NamedResult{{Name: "beads", Result: Result{Status: StatusOK, Items: []Item{bead}}}, {Name: "codex-goals", Result: Result{Status: StatusOK, Items: []Item{goal}, NativeScopes: map[string]NativeScope{home: {Complete: true}}}}}}}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	initial, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	beadAlias := "beads:" + home + ":" + bead.Ref.ID
	goalAlias := "codex-goals:" + home + ":" + goal.Ref.ID
	beadID, goalID := initial.Aliases[beadAlias], initial.Aliases[goalAlias]
	if _, err = store.Apply(ctx, OwnerAction{Kind: "cancel", Scope: "task", TaskID: beadID}); err != nil {
		t.Fatal(err)
	}
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Consolidate(ctx, Consolidation{Survivor: goalID, Absorb: []string{beadID}, Authority: beadAlias, ExpectedGeneration: state.Generation - 1}); err == nil {
		t.Fatal("stale identity correction accepted")
	}
	canonical, err := store.Consolidate(ctx, Consolidation{Survivor: goalID, Absorb: []string{beadID}, Authority: beadAlias, ExpectedGeneration: state.Generation})
	if err != nil {
		t.Fatal(err)
	}
	consolidated, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if canonical != goalID || consolidated.Tasks[beadID].RedirectTo != goalID || !consolidated.Tasks[goalID].Cancelled || len(consolidated.Facts) != 2 || len(consolidated.Catalogue()) != 1 {
		t.Fatal("consolidation lost source facts, copied IDs or cancellation")
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "reopen", Scope: "task", TaskID: beadID}); err != nil {
		t.Fatal(err)
	}
	// Reverse source order and change the secondary source; chosen authority
	// and outcome text remain owned by the task.
	acquisition.Input.Results[0], acquisition.Input.Results[1] = acquisition.Input.Results[1], acquisition.Input.Results[0]
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if replay.Tasks[goalID].Authority != beadAlias || replay.Tasks[goalID].Title != "Chosen outcome" || replay.Tasks[goalID].Fact.State != StateActive || replay.Tasks[goalID].Cancelled {
		t.Fatal("source replay changed chosen authority, title or explicit reopen")
	}
	model := replay.Project(DefaultConfig(), time.Now(), "test")
	found := false
	for _, column := range model.Columns {
		for _, card := range column.Cards {
			if card.ID == goalID {
				found = len(card.SourceFacts) == 2 && len(card.Items) == 2 && len(card.Marks) > 0
			}
		}
	}
	if !found {
		t.Fatal("source disagreements unavailable to common projection")
	}
	// Add a closed destination. Moving the chosen alias requires replacement,
	// keeps the donor task, and does not copy its cancellation to another outcome.
	otherRef, _ := Bead("demo-destination")
	other := Item{Ref: otherRef, Title: "Destination outcome", State: StateDone, Structural: true}
	for i := range acquisition.Input.Results {
		if acquisition.Input.Results[i].Name == "beads" {
			acquisition.Input.Results[i].Result.Items = []Item{bead, other}
		}
	}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	moveState, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	destination := moveState.Aliases["beads:"+home+":"+otherRef.ID]
	if _, err = store.MoveAlias(ctx, AliasMove{Alias: beadAlias, Destination: destination, ExpectedGeneration: moveState.Generation}); err == nil {
		t.Fatal("moving authority accepted without replacement")
	}
	if _, err = store.MoveAlias(ctx, AliasMove{Alias: beadAlias, Destination: destination, DonorAuthority: "owner", DestinationAuthority: beadAlias, ExpectedGeneration: moveState.Generation}); err != nil {
		t.Fatal(err)
	}
	moved, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if moved.Tasks[goalID].RedirectTo != "" || moved.Tasks[goalID].Authority != "operator" || moved.Tasks[goalID].Fact.State == StateDone || moved.Aliases[beadAlias] != destination || moved.Anchors[beadAlias] != destination {
		t.Fatal("alias move deleted/completed donor or failed to update replay binding")
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "cancel", Scope: "task", TaskID: goalID}); err != nil {
		t.Fatal(err)
	}
	next, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Consolidate(ctx, Consolidation{Survivor: destination, Absorb: []string{goalID}, Authority: beadAlias, ExpectedGeneration: next.Generation}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if final.Tasks[beadID].RedirectTo != destination || final.Tasks[goalID].RedirectTo != destination {
		t.Fatal("redirects were not flattened")
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "reopen", Scope: "task", TaskID: beadID}); err != nil {
		t.Fatal(err)
	}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Tasks[destination].Cancelled || len(reopened.Catalogue()) != 1 || len(reopened.Tasks) != 3 {
		t.Fatal("historical redirected cancellation overrode canonical reopen")
	}
	// Authority review: source links belong to each alias, not the selected
	// task fact. Consolidated Beads must keep both exact source commands.
	reopened.Input.Known[otherRef.ID] = "closed"
	links := map[string]bool{}
	commands := map[string]bool{}
	for _, column := range reopened.Project(DefaultConfig(), time.Now(), "test").Columns {
		for _, card := range column.Cards {
			if card.ID != destination {
				continue
			}
			for _, ref := range card.Refs {
				links[ref.ID] = true
			}
			for _, action := range card.Actions {
				if action.Purpose == ActionContext && action.Command != "" {
					commands[action.Command] = true
				}
			}
		}
	}
	for _, ref := range []Ref{beadRef, otherRef} {
		want := newAction("show bead", "bd", "--readonly", "--sandbox", "--db", home, "show", ref.ID)
		if !links[ref.ID] || !commands[want.Command] {
			t.Fatalf("consolidated alias lost exact source link: %s", ref.ID)
		}
	}
}

// The final-wave contract keeps old discoveries outside NEW work, binds model
// selections to exact source ranges, and requires an explicit owner adoption.
func checkSuggestionAdoption(t *testing.T, ctx context.Context) {
	t.Helper()
	checkReferencedSuggestionCandidates(t)
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
	initial, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exported := func(id, kind, body, created string) (Message, []byte) {
		entry := thread.Entry{ID: id, Thread: "episode-" + id, Kind: kind, Body: body, Created: created}
		raw, marshalErr := json.Marshal(entry)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		sum := sha256.Sum256(raw)
		return MessageFromEntry(base, base, "", entry, hex.EncodeToString(sum[:])), raw
	}
	old, oldRaw := exported("old", "todo", "Implement the archived report format.\n\nThis note supplies historical context.", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	acquisition := Acquisition{ProjectID: initial.ProjectID, Messages: []Message{old}, Evidence: [][]byte{oldRaw}, Input: Input{Results: []NamedResult{{Name: "amq-history", Result: Result{Status: StatusOK, ActionStates: map[string]string{old.Key: "open"}}}}}}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	baseline, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.HistoryBaseline.IsZero() || !baseline.Observations[old.Key].Bootstrap {
		t.Fatal("complete-history bootstrap was not retained")
	}
	current, currentRaw := exported("current", "todo", "Implement a local export command.", baseline.HistoryBaseline.Add(time.Nanosecond).Format(time.RFC3339Nano))
	acquisition.Messages = append(acquisition.Messages, current)
	acquisition.Evidence = append(acquisition.Evidence, currentRaw)
	acquisition.Input.Results[0].Result.ActionStates[current.Key] = "open"
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldUnits, err := unitsFor(state, old.Key)
	if err != nil {
		t.Fatal(err)
	}
	if newEligible(state, oldUnits[0], time.Now()) {
		t.Fatal("bootstrap history became current NEW work")
	}
	units, err := unitsFor(state, current.Key)
	if err != nil {
		t.Fatal(err)
	}
	// Review measured 2,608 unrelated messages in one P2P thread. Exact
	// reference context keeps the source whole and reports the omitted history.
	shared := state
	shared.Messages = make(map[string]Message, len(state.Messages)+1)
	for k, m := range state.Messages {
		shared.Messages[k] = m
	}
	noise := old
	noise.Key = base + "#unrelated"
	noise.ID = "unrelated"
	noise.Thread = current.Thread
	shared.Messages[noise.Key] = noise
	episode, episodeErr := buildEpisode(shared, units)
	if episodeErr != nil || episode.Included != 1 || episode.Omitted != 1 || !episode.Partial || episode.Messages[0].Body != current.Body || newEligible(shared, units[0], time.Now()) {
		t.Fatalf("shared thread context was imported or NEW ignored its gap: %v", episodeErr)
	}

	_, choices := buildDecision(state, units, true, time.Now())
	if choices["new_0"].Kind != "new" {
		t.Fatal("current independent ask lacked positive NEW choice")
	}
	cfg := decision.Config{}
	fp := state.SuggestionFingerprint(cfg)
	proposal := makeSuggestion(fp, units[0], []string{"new_0"}, choices)
	if err = store.publishSuggestions(ctx, state, cfg, units, true, []Suggestion{proposal}, SuggestionAttempt{Fingerprint: fp, Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	published, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if published.SuggestionFingerprint(cfg) != fp || len(published.Tasks) != 0 || len(published.SuggestionViews()[0].Commands) != 2 {
		t.Fatal("suggestion publication changed its own fingerprint or admitted work")
	}
	adopted, err := store.AdoptSuggestion(ctx, SuggestionAdoption{ID: proposal.ID, Choices: proposal.Selected, ExpectedGeneration: published.Generation, ConfirmIndependent: true})
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := after.Tasks[adopted[0]]
	if len(adopted) != 1 || !task.Inferred || task.Fact.State != StateRequested || len(task.ModelAttachments) != 1 || task.ModelAttachments[0].Unit.Quote != current.Body || len(after.Project(DefaultConfig(), time.Now(), "test").Unresolved) != 2 {
		t.Fatal("adoption lost exact evidence or hid unsupported source text")
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "cancel", Scope: "task", TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	after, err = store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.AdoptSuggestion(ctx, SuggestionAdoption{ID: proposal.ID, Choices: proposal.Selected, ExpectedGeneration: after.Generation, ConfirmIndependent: true})
	if err != nil || len(replay) != 1 || replay[0] != task.ID {
		t.Fatalf("adoption replay replaced permanent identity: %v", err)
	}
	after, err = store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, choices = buildDecision(after, oldUnits, false, time.Now())
	first := makeSuggestion(after.SuggestionFingerprint(cfg), oldUnits[0], []string{"t0_evidence"}, choices)
	second := makeSuggestion(after.SuggestionFingerprint(cfg), oldUnits[1], []string{"non_actionable"}, choices)
	if err = store.publishSuggestions(ctx, after, cfg, oldUnits, false, []Suggestion{first, second}, SuggestionAttempt{Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	published, err = store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdoptSuggestion(ctx, SuggestionAdoption{ID: first.ID, Choices: first.Selected, ExpectedGeneration: published.Generation}); err != nil {
		t.Fatal(err)
	}
	after, err = store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.IgnoreSuggestion(ctx, second.ID, after.Generation); err != nil {
		t.Fatal(err)
	}
	final, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Tasks[task.ID].Cancelled || len(final.Tasks[task.ID].ModelAttachments) != 2 || final.Dismissed[old.Key] || !final.SuggestionIgnored[oldUnits[1].ID] {
		t.Fatal("adoption/ignore changed source disposition or cleared cancellation")
	}
	model := final.Project(DefaultConfig(), time.Now(), "test")
	if len(model.Historical) != 1 || model.Historical[0].HistoryStatus != "owner-cancelled" || len(model.Next) != 0 || len(model.Unresolved) != 2 {
		t.Fatal("noncurrent inferred task entered current work or hid source remainder")
	}
	if err = store.publishSuggestions(ctx, state, cfg, units, true, []Suggestion{proposal}, SuggestionAttempt{}); err == nil {
		t.Fatal("stale model result published after owner changes")
	}
	// Final review: retained redirected donors cannot suppress evidence after a
	// canonical reopen/detach, or keep an old cancellation effective.
	destination, err := store.Apply(ctx, OwnerAction{Kind: "admit", MessageKey: old.Key, Anchor: "span-destination", Title: "Independent destination"})
	if err != nil {
		t.Fatal(err)
	}
	beforeCorrection, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Consolidate(ctx, Consolidation{Survivor: destination, Absorb: []string{task.ID}, Authority: "owner", ExpectedGeneration: beforeCorrection.Generation}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "reopen", Scope: "task", TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Apply(ctx, OwnerAction{Kind: "detach", TaskID: destination, MessageKey: current.Key}); err != nil {
		t.Fatal(err)
	}
	detached, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := unitsFor(detached, current.Key)
	if err != nil || len(pending) != 1 || !newEligible(detached, pending[0], time.Now()) {
		t.Fatalf("retained donor still governed detached evidence: %v", err)
	}
	_, choices = buildDecision(detached, pending, false, time.Now())
	attachChoice := ""
	for id, c := range choices {
		if c.Kind == "attach" && c.TaskID == destination && c.Role == "evidence" {
			attachChoice = id
		}
	}
	spanProposal := makeSuggestion(detached.SuggestionFingerprint(cfg), pending[0], []string{attachChoice}, choices)
	if err = store.publishSuggestions(ctx, detached, cfg, pending, false, []Suggestion{spanProposal}, SuggestionAttempt{Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	published, err = store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdoptSuggestion(ctx, SuggestionAdoption{ID: spanProposal.ID, Choices: spanProposal.Selected, ExpectedGeneration: published.Generation}); err != nil {
		t.Fatal(err)
	}
	beadRef, _ := Bead("demo-span")
	sourceStore := t.TempDir()
	acquisition.BeadsStore = sourceStore
	acquisition.Input.Results = append(acquisition.Input.Results, NamedResult{Name: "beads", Result: Result{Status: StatusOK, Items: []Item{{Ref: beadRef, Title: "Native destination", State: StateActive, Structural: true}}}})
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	withNative, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias := "beads:" + sourceStore + ":" + beadRef.ID
	nativeID := withNative.Aliases[alias]
	// Final review: moving only adopted spans must not create a whole-message
	// binding and hide its unsupported remainder, through either owner command.
	if _, err = store.Apply(ctx, OwnerAction{Kind: "move", FromTaskID: destination, TaskID: nativeID, MessageKey: current.Key}); err != nil {
		t.Fatal(err)
	}
	spanMoved, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(spanMoved.Tasks[nativeID].Attachments) != 0 || len(spanMoved.Tasks[nativeID].ModelAttachments) != 1 || len(spanMoved.Project(DefaultConfig(), time.Now(), "test").Unresolved) != 1 {
		t.Fatal("span-only move promoted evidence to whole-message attachment")
	}
	if _, err = store.MoveAlias(ctx, AliasMove{Alias: alias, Destination: destination, DonorAuthority: "owner", Attachments: []string{current.Key}, ExpectedGeneration: spanMoved.Generation}); err != nil {
		t.Fatal(err)
	}
	aliasMoved, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range aliasMoved.Tasks[destination].Attachments {
		if key == current.Key {
			t.Fatal("alias move promoted span to whole-message attachment")
		}
	}
	if len(aliasMoved.Tasks[nativeID].ModelAttachments) != 0 || len(aliasMoved.Tasks[destination].ModelAttachments) != 2 || len(aliasMoved.Project(DefaultConfig(), time.Now(), "test").Unresolved) != 1 {
		t.Fatal("alias move lost spans or source remainder")
	}
	// Final review: an oversized oldest message retains its source key in a
	// failed attempt and does not starve the following automatic iteration.
	large, largeRaw := exported("large", "todo", strings.Repeat("A source paragraph.\n\n", 9), time.Now().UTC().Format(time.RFC3339Nano))
	acquisition.Messages = append(acquisition.Messages, large)
	acquisition.Evidence = append(acquisition.Evidence, largeRaw)
	acquisition.Input.Results[0].Result.ActionStates[large.Key] = "open"
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	beforeFailure, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failedUnits, planErr := unitsFor(beforeFailure, "")
	if planErr == nil || len(failedUnits) != 8 || failedUnits[0].MessageKey != large.Key {
		t.Fatal("oversized episode lost selected message key")
	}
	if err = store.publishSuggestions(ctx, beforeFailure, cfg, failedUnits, false, nil, SuggestionAttempt{Status: "unavailable", Detail: planErr.Error()}); err != nil {
		t.Fatal(err)
	}
	failed, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Suggestions.Attempt.MessageKey != large.Key || failed.Observations[large.Key].AttemptFingerprint != failed.SuggestionFingerprint(cfg) {
		t.Fatal("failed episode attempt was not recorded")
	}
	remaining, nextErr := unitsFor(failed, "")
	if nextErr != nil || len(remaining) != 0 {
		t.Fatal("unchanged failed episode stalled the worker")
	}
	// Live board: a status message was rejected before the model could attach it.
	status, statusRaw := exported("status", "status", "The local export command is ready for review.", time.Now().UTC().Format(time.RFC3339Nano))
	acquisition.Messages = append(acquisition.Messages, status)
	acquisition.Evidence = append(acquisition.Evidence, statusRaw)
	acquisition.Input.Results[0].Result.ActionStates[status.Key] = "resolved"
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	withStatus, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusUnits, err := unitsFor(withStatus, status.Key)
	if err != nil || len(statusUnits) != 1 || statusUnits[0].Quote != status.Body || newEligible(withStatus, statusUnits[0], time.Now()) {
		t.Fatalf("status evidence must be selectable without becoming NEW work: %v", err)
	}
}

func checkReferencedSuggestionCandidates(t *testing.T) {
	t.Helper()
	// Board pilot from agent-message-queue-yok: literal task IDs and a direct
	// review reply lost their tasks outside the initial eight model choices.
	for _, tc := range []struct {
		name, body string
		targets    map[string]string
		opener     string
	}{
		{"continuation", "Start demo-611.45 now, as briefed earlier.", map[string]string{"demo-611.45": "Persist owed outcomes"}, ""},
		{"compound", "Fix demo-lfd.\n\nThen fix demo-4sc.", map[string]string{"demo-lfd": "Sanitize refusal text", "demo-4sc": "Shorten PTY tests"}, ""},
		{"docs", "New work: demo-docs. Edit the CLI skills README.", map[string]string{"demo-docs": "Codex invokes dollar skills"}, ""},
		{"review reply", "GO. Waiting for final CI.", map[string]string{"demo-g4": "Hold the doorbell while agy shows a permission or sign-in prompt"}, "Hold the doorbell while agy shows a permission or sign-in prompt. Review it."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := Snapshot{Tasks: map[string]Task{}, Facts: map[string]AliasFact{}, Messages: map[string]Message{}}
			addTask := func(id, title string) {
				alias := "beads:/scope:" + id
				state.Tasks[id] = Task{ID: id, Title: title, Authority: alias, Aliases: []string{alias}, Cancelled: true}
				state.Facts[alias] = AliasFact{Alias: alias, Fact: Item{Ref: Ref{Kind: RefBead, ID: id}, Title: title, State: StateDone}}
			}
			for i := range 10 {
				addTask(fmt.Sprintf("noise-%d", i), "Start now as briefed earlier fix then new work edit CLI skills README GO waiting final CI")
			}
			for id, title := range tc.targets {
				addTask(id, title)
			}
			message := Message{Key: "source#reply", ID: "reply", Namespace: "source", Kind: "review_response", Body: tc.body}
			if tc.opener != "" {
				message.Refs = []string{"opener"}
				state.Messages["source#opener"] = Message{Key: "source#opener", ID: "opener", Namespace: "source", Body: tc.opener}
			}
			state.Messages[message.Key] = message
			units, err := unitsFor(state, message.Key)
			if err != nil {
				t.Fatal(err)
			}
			_, choices := buildDecision(state, units, false, time.Now())
			for id := range tc.targets {
				found := false
				for _, choice := range choices {
					found = found || choice.Kind == "attach" && choice.TaskID == id
				}
				if !found {
					t.Errorf("referenced task %s is absent from the initial model choices", id)
				}
			}
		})
	}
}
