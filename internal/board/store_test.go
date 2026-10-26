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
