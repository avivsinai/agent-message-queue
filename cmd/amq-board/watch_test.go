package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

// A watch pass that fails keeps the loop alive: the error prints once per
// distinct text, the floor step still runs after every pass, and a later
// success publishes. The fleet's sync --watch stopped on one failed pass and
// nothing noticed for 40 hours (field, 2026-11-23).
func TestWatchLoopContinuesAfterFailedPassAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log bytes.Buffer
	passErr := errors.New("amq-board sync: acquisition failed")
	calls, floors := 0, 0
	pass := func(context.Context) (board.Snapshot, error) {
		calls++
		switch calls {
		case 1, 2:
			return board.Snapshot{}, passErr
		case 3:
			return board.Snapshot{Generation: 7, Tasks: map[string]board.Task{"t": {ID: "t"}}}, nil
		}
		t.Fatal("pass called after completion")
		return board.Snapshot{}, nil
	}
	waits := 0
	code := watchLoop(ctx, true, pass, func(context.Context) { floors++ }, func(context.Context) bool {
		waits++
		if waits == 3 {
			cancel()
			return false
		}
		return true
	}, &log)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if floors != 3 {
		t.Fatalf("floor passes = %d, want 3", floors)
	}
	got := log.String()
	if n := strings.Count(got, passErr.Error()); n != 1 {
		t.Fatalf("error line count = %d, want 1: %q", n, got)
	}
	if n := strings.Count(got, "amq-board: published generation 7, 1 tasks"); n != 1 {
		t.Fatalf("published line count = %d, want 1: %q", n, got)
	}
	if n := strings.Count(got, "the next pass tries again"); n != 1 {
		t.Fatalf("retry line count = %d, want 1: %q", n, got)
	}
}

// A replaced project ends the watch: the process is bound to a store that is
// gone, so it exits once, with one line, and never runs the floor again.
func TestWatchLoopEndsOnProjectChanged(t *testing.T) {
	var log bytes.Buffer
	passErr := board.ErrProjectChanged
	floors := 0
	pass := func(context.Context) (board.Snapshot, error) {
		return board.Snapshot{}, passErr
	}
	code := watchLoop(context.Background(), true, pass, func(context.Context) { floors++ }, func(context.Context) bool { return true }, &log)
	if code != exitGeneral {
		t.Fatalf("exit code = %d, want %d", code, exitGeneral)
	}
	if floors != 0 {
		t.Fatalf("floor passes = %d, want 0", floors)
	}
	if got := log.String(); got != passErr.Error()+"\n" {
		t.Fatalf("log = %q, want the one project-changed line", got)
	}
}

// One-shot sync keeps main's contract: a failing pass exits 1 with its line,
// and a good pass exits 0.
func TestOneShotSyncFailingAndGoodPass(t *testing.T) {
	var log bytes.Buffer
	passErr := errors.New("amq-board sync: acquisition failed")
	code := watchLoop(context.Background(), false, func(context.Context) (board.Snapshot, error) {
		return board.Snapshot{}, passErr
	}, func(context.Context) { t.Fatal("floor ran in one-shot mode") }, func(context.Context) bool {
		t.Fatal("wait ran in one-shot mode")
		return false
	}, &log)
	if code != exitGeneral {
		t.Fatalf("failing one-shot exit = %d, want %d", code, exitGeneral)
	}
	if got := log.String(); got != passErr.Error()+"\n" {
		t.Fatalf("failing one-shot log = %q", got)
	}
	log.Reset()
	code = watchLoop(context.Background(), false, func(context.Context) (board.Snapshot, error) {
		return board.Snapshot{Generation: 1}, nil
	}, func(context.Context) { t.Fatal("floor ran in one-shot mode") }, func(context.Context) bool {
		t.Fatal("wait ran in one-shot mode")
		return false
	}, &log)
	if code != 0 {
		t.Fatalf("good one-shot exit = %d, want 0", code)
	}
	if got := log.String(); !strings.Contains(got, "published generation 1") {
		t.Fatalf("good one-shot log = %q", got)
	}
}

// floorStep with no recipients returns nil, and a watch loop with a nil
// floor step runs its passes and waits without a floor notice.
func TestFloorStepNilWithoutRecipientsAndWatchLoopProceeds(t *testing.T) {
	if floorStep(&syncRun{}, "", nil, io.Discard) != nil {
		t.Fatal("floorStep with no recipients = non-nil, want nil")
	}
	var log bytes.Buffer
	calls, waits := 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := watchLoop(ctx, true, func(context.Context) (board.Snapshot, error) {
		calls++
		return board.Snapshot{Generation: uint64(calls)}, nil
	}, nil, func(context.Context) bool {
		waits++
		if waits == 2 {
			cancel()
			return false
		}
		return true
	}, &log)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if calls != 2 || waits != 2 {
		t.Fatalf("passes = %d, waits = %d, want 2 and 2", calls, waits)
	}
	if n := strings.Count(log.String(), "published generation"); n != 2 {
		t.Fatalf("published lines = %d, want 2: %q", n, log.String())
	}
}
