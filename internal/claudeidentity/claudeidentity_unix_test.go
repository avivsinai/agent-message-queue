//go:build !windows

package claudeidentity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// Purpose: TestBindThenLookupReturnsTheIdentity guards that a bound Claude
// session finds its identity again, that rebinding is idempotent, and that
// store files are private. TestASecondSessionCannotTakeAHeldHandle guards
// that one handle has one live Claude session until it releases, and that a
// release leaves a tombstone. TestRebindingASessionFreesItsOldHandle guards
// that a session that moves to another handle stops holding the old one.
// TestAHolderWhoseRecordCannotBeReadKeepsItsHandle is the regression test
// for a pre-merge review finding: a running holder whose record had a bad
// mode lost its handle to the next join, with no notice.

const (
	sessA = "0190a1f0-1111-7000-8000-00000000000a"
	sessB = "0190a1f0-2222-7000-8000-00000000000b"
)

// running says every holder still runs, so a held handle is refused.
func running(Record) bool { return true }

func rec(home, session, me string) Record {
	root := filepath.Join(home, "q")
	return Record{
		ClaudeSession: session,
		ConfigDir:     filepath.Join(home, ".claude"),
		Identity: launch.Identity{
			Root: root, BaseRoot: root, Me: me,
			RootID: "v1:darwin:1000011:278027", BaseRootID: "v1:darwin:1000011:278027",
		},
	}
}

func TestBindThenLookupReturnsTheIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	r := rec(home, sessA, "reviewer")
	if _, err := Bind(r, running); err != nil {
		t.Fatal(err)
	}
	if _, err := Bind(r, running); err != nil {
		t.Fatalf("binding the same identity again: %v", err)
	}
	got, err := Lookup(sessA, r.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity != r.Identity || got.ClaudeSession != sessA {
		t.Fatalf("got %+v, want identity %+v", got, r.Identity)
	}
	if _, err := Lookup(sessB, r.ConfigDir); !errors.Is(err, ErrNone) {
		t.Fatalf("unknown session: got %v, want ErrNone", err)
	}
	var files []string
	_ = filepath.Walk(filepath.Join(home, ".amq", "claude-sessions"), func(p string, fi os.FileInfo, _ error) error {
		if fi != nil && fi.Mode().IsRegular() && filepath.Base(p) != ".lock" {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 2 {
		t.Fatalf("store files %v, want a record and a holder marker", files)
	}
	for _, p := range files {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", p, fi.Mode().Perm())
		}
	}
}

func TestASecondSessionCannotTakeAHeldHandle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a, b := rec(home, sessA, "reviewer"), rec(home, sessB, "reviewer")
	if _, err := Bind(a, running); err != nil {
		t.Fatal(err)
	}
	var held *HeldError
	if _, err := Bind(b, running); !errors.As(err, &held) || held.ClaudeSession != sessA {
		t.Fatalf("got %v, want HeldError naming %s", err, sessA)
	}
	if _, err := Lookup(sessB, b.ConfigDir); !errors.Is(err, ErrNone) {
		t.Fatalf("refused session left a record: %v", err)
	}
	if _, err := Lookup(sessA, a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	left, freed, err := Release(sessA, a.ConfigDir)
	if err != nil || !freed || left.Me != "reviewer" || left.Released {
		t.Fatalf("release returned %+v, freed %v, %v", left, freed, err)
	}
	if _, err := Lookup(sessA, a.ConfigDir); !errors.Is(err, ErrNone) {
		t.Fatalf("released session: got %v, want ErrNone", err)
	}
	if _, err := os.Stat(filepath.Join(SessionsDir(home), sessA+".json")); err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}
	if _, _, err := Release(sessA, a.ConfigDir); !errors.Is(err, ErrNone) {
		t.Fatalf("second release: got %v, want ErrNone", err)
	}
	if _, err := Bind(b, running); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup(sessB, b.ConfigDir); err != nil {
		t.Fatal(err)
	}
}

func TestRebindingASessionFreesItsOldHandle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := Bind(rec(home, sessA, "reviewer"), running); err != nil {
		t.Fatal(err)
	}
	a := rec(home, sessA, "builder")
	if _, err := Bind(a, running); err != nil {
		t.Fatal(err)
	}
	got, err := Lookup(sessA, a.ConfigDir)
	if err != nil || got.Me != "builder" {
		t.Fatalf("got %+v, %v, want builder", got, err)
	}
	if _, err := Bind(rec(home, sessB, "reviewer"), running); err != nil {
		t.Fatalf("the old handle was not freed: %v", err)
	}
}

func TestAHolderWhoseRecordCannotBeReadKeepsItsHandle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := rec(home, sessA, "reviewer")
	if _, err := Bind(a, running); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".amq", "claude-sessions", "sessions", sessA+".json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	var unreadable *UnreadableHolderError
	ended := func(Record) bool { return false }
	if _, err := Bind(rec(home, sessB, "reviewer"), ended); !errors.As(err, &unreadable) || unreadable.Path != path {
		t.Fatalf("Bind over a holder whose record cannot be read = %v, want an error naming %s", err, path)
	}
	if _, _, err := Holder(a.Root, a.RootID, a.Me); !errors.As(err, &unreadable) {
		t.Fatalf("Holder = %v, want the unreadable record", err)
	}
}
