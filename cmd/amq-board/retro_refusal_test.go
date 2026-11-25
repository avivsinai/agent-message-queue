package main

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

// Field, 2026-11-25: the board showed a 40-hour-old snapshot as current; old retro
// omitted 110 recent requests and still reported success. Retro uses check's exact
// publication refusal instead.
func TestRetroRefusesTheSameOldSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	state := board.Snapshot{Generation: 8571, PublishedAt: now.Add(-40 * time.Hour), Input: board.Input{Config: board.DefaultConfig()}}
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	code := retroPublished(state, board.FileConfig{Board: board.DefaultConfig()}, "72h", false, now)
	_ = w.Close()
	os.Stderr = oldStderr
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if code != exitGeneral {
		t.Fatalf("retro exit=%d want 1", code)
	}
	if string(got) != "amq-board retro: the board snapshot is 40h old; run amq-board sync first\n" {
		t.Fatalf("stderr=%q", got)
	}
}
