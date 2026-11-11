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

const (
	sessA = "0190a1f0-1111-7000-8000-00000000000a"
	sessB = "0190a1f0-2222-7000-8000-00000000000b"
)

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
	if err := Bind(r); err != nil {
		t.Fatal(err)
	}
	if err := Bind(r); err != nil {
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
	if err := Bind(a); err != nil {
		t.Fatal(err)
	}
	var held *HeldError
	if err := Bind(b); !errors.As(err, &held) || held.ClaudeSession != sessA {
		t.Fatalf("got %v, want HeldError naming %s", err, sessA)
	}
	if _, err := Lookup(sessB, b.ConfigDir); !errors.Is(err, ErrNone) {
		t.Fatalf("refused session left a record: %v", err)
	}
	if _, err := Lookup(sessA, a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	left, err := Release(sessA, a.ConfigDir)
	if err != nil || left.Me != "reviewer" || left.Released {
		t.Fatalf("release returned %+v, %v", left, err)
	}
	if _, err := Lookup(sessA, a.ConfigDir); !errors.Is(err, ErrNone) {
		t.Fatalf("released session: got %v, want ErrNone", err)
	}
	if _, err := os.Stat(filepath.Join(SessionsDir(home), sessA+".json")); err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}
	if _, err := Release(sessA, a.ConfigDir); !errors.Is(err, ErrNone) {
		t.Fatalf("second release: got %v, want ErrNone", err)
	}
	if err := Bind(b); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup(sessB, b.ConfigDir); err != nil {
		t.Fatal(err)
	}
}

func TestRebindingASessionFreesItsOldHandle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := Bind(rec(home, sessA, "reviewer")); err != nil {
		t.Fatal(err)
	}
	a := rec(home, sessA, "builder")
	if err := Bind(a); err != nil {
		t.Fatal(err)
	}
	got, err := Lookup(sessA, a.ConfigDir)
	if err != nil || got.Me != "builder" {
		t.Fatalf("got %+v, %v, want builder", got, err)
	}
	if err := Bind(rec(home, sessB, "reviewer")); err != nil {
		t.Fatalf("the old handle was not freed: %v", err)
	}
}
