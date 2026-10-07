package pi

import (
	"os"
	"path/filepath"
	"testing"
)

// Review of #978 (Pro, P1): with the target pinned to session A, a symlink
// activity/A.jsonl -> B.jsonl made session B's records publish as A. The
// tail reads only a regular file and only records that name their session.
func TestPiActivityTailReadsOnlyThePinnedSession(t *testing.T) {
	dir := t.TempDir()
	activity := filepath.Join(dir, "activity")
	if err := os.Mkdir(activity, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"protocol":"` + piWire.protocol + `","session":"B","kind":"assistant","text":"secret of B"}` + "\n"
	if err := os.WriteFile(filepath.Join(activity, "B.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(activity, "B.jsonl"), filepath.Join(activity, "A.jsonl")); err != nil {
		t.Fatal(err)
	}
	a := &Attachment{dir: bridgeDir{dir: dir, names: piWire}}
	var got []ActivityNote
	a.tailActivity("A", 0, func(n ActivityNote) { got = append(got, n) })
	if err := os.Remove(filepath.Join(activity, "A.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(activity, "A.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	a.tailActivity("A", 0, func(n ActivityNote) { got = append(got, n) })
	if len(got) != 0 {
		t.Fatalf("pinned session A received %+v; want nothing from session B", got)
	}
}
