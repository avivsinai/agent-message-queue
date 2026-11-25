package board

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"strings"
	"testing"
	"time"
)

// wow-teardown 2026-11-25: a stopped watch left 40-hour-old cap and seat facts
// looking current. All read-only faces age that same publication, without a
// source read or a new synchronous status check.
func TestSnapshotAgingReachesTextJSONPaneAndHTML(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := Snapshot{Schema: SnapshotSchema, Generation: 8571, PublishedAt: now.Add(-40 * time.Hour), Input: Input{Config: DefaultConfig()}}
	dir := t.TempDir()
	identity, err := fsq.StableTreeIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.BaseRoot, s.BaseRootID, s.ProjectID = dir, identity, "copy"
	s.Tasks, s.Anchors, s.Aliases, s.Messages = map[string]Task{}, map[string]string{}, map[string]string{}, map[string]Message{}
	s.Dismissed, s.ActionCancelled, s.Withdrawals, s.Facts = map[string]bool{}, map[string]bool{}, map[string]string{}, map[string]AliasFact{}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stored, err := (Store{Dir: dir, BaseRoot: dir, BaseRootID: identity}).Read()
	if err != nil {
		t.Fatal(err)
	}
	m := stored.Project(DefaultConfig(), now, "test")
	if !m.Stale || m.PublishedAgeSeconds != 40*60*60 {
		t.Fatalf("stale=%v age=%d", m.Stale, m.PublishedAgeSeconds)
	}
	pane, err := PaneJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Stale bool  `json:"stale"`
		Age   int64 `json:"published_age_seconds"`
	}
	if err := json.Unmarshal(pane, &doc); err != nil || !doc.Stale || doc.Age != 40*60*60 {
		t.Fatalf("pane=%s", pane)
	}
	var page bytes.Buffer
	if err := WriteHTML(&page, m, "regenerate"); err != nil {
		t.Fatal(err)
	}
	wanted := "STALE: published 40h ago (2026-11-23T20:00:00Z). Every number below is from then. Run amq-board sync."
	if !strings.Contains(page.String(), wanted) {
		t.Fatalf("no old publication banner")
	}
}
