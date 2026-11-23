package dialog_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// signedDigest computes what the server signed for a dialog's record:
// the digest of its rendered interaction, the value the write must match.
func signedDigest(t *testing.T, dir, id string) string {
	t.Helper()
	_, req, _, _, err := dialog.Lookup(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	in, ok := dialog.RenderLinkInteraction(req)
	if !ok {
		t.Fatalf("dialog %s renders no link interaction", id)
	}
	d, err := dialog.InteractionDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// openDialog writes one open dialog record with the two link choices and
// returns its id and directory.
func openDialog(t *testing.T, dir string) string {
	t.Helper()
	for _, sub := range []string{"answers", "requests"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id := dialog.NewID()
	if err := dialog.Open(dir, dialog.Request{ID: id, Harness: "claude", Session: "pi:chat",
		Tool: "Bash", Text: "echo hi", Complete: true, OpenedAt: time.Now().UTC(),
		Options: []dialog.Option{
			{N: 1, Label: "Allow once", Value: "allow", Scope: dialog.ScopeOnce},
			{N: 2, Label: "Deny", Value: "deny", Deny: true, Scope: dialog.ScopeOnce},
			{N: 3, Label: "Always allow", Value: "allow", Scope: dialog.ScopeSession},
		}}); err != nil {
		t.Fatal(err)
	}
	return id
}

// The happy path: the answer file holds the chosen option's number, by
// link, the document digest and the credential; the request file and the
// temp names are untouched; the file is 0600.
func TestWriteLinkAnswerRecordsTheChoice(t *testing.T) {
	dir := t.TempDir()
	id := openDialog(t, dir)
	if err := dialog.WriteLinkAnswer(dir, id, "Allow once", dialog.RoleApprove, signedDigest(t, dir, id), "sha256:abc", "cred-1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	phase, req, a, _, err := dialog.Lookup(dir, id)
	if err != nil || phase != dialog.PhaseAnswered {
		t.Fatalf("phase = %s (%v); want answered", phase, err)
	}
	if a == nil || a.Option != 1 || a.By != dialog.ByLink || a.DocumentDigest != "sha256:abc" || a.Credential != "cred-1" {
		t.Fatalf("answer = %+v; want option 1, by link, the digest and credential", a)
	}
	if len(req.Options) != 3 {
		t.Fatalf("request options = %d; want the record untouched", len(req.Options))
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "answers"))
	if len(entries) != 1 || entries[0].Name() != id+".json" {
		t.Fatalf("answers entries = %v; want only the answer file", entries)
	}
	fi, err := os.Stat(filepath.Join(dir, "answers", id+".json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("answer file mode = %v (%v); want 0600", fi.Mode(), err)
	}
}

// The deny role admits only the deny choice.
func TestWriteLinkAnswerDenyRole(t *testing.T) {
	dir := t.TempDir()
	id := openDialog(t, dir)
	if err := dialog.WriteLinkAnswer(dir, id, "Deny", dialog.RoleDeny, signedDigest(t, dir, id), "sha256:d", "cred-1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, a, _, err := dialog.Lookup(dir, id)
	if err != nil || a == nil || a.Option != 2 {
		t.Fatalf("answer = %+v (%v); want option 2", a, err)
	}
}

// Refusals: a role with no matching choice, an unknown label, an
// out-of-scope label, a missing answers directory, a missing request file,
// and a resolution — each records nothing.
func TestWriteLinkAnswerRefusals(t *testing.T) {
	t.Run("role mismatch", func(t *testing.T) {
		dir := t.TempDir()
		id := openDialog(t, dir)
		// The approve label with the deny role names no deny choice.
		err := dialog.WriteLinkAnswer(dir, id, "Allow once", dialog.RoleDeny, signedDigest(t, dir, id), "sha256:x", "c")
		if !errors.Is(err, dialog.ErrLinkNoSuchChoice) {
			t.Fatalf("approve label as deny = %v; want no such choice", err)
		}
		// The deny label with the approve role, the same way.
		err = dialog.WriteLinkAnswer(dir, id, "Deny", dialog.RoleApprove, signedDigest(t, dir, id), "sha256:x", "c")
		if !errors.Is(err, dialog.ErrLinkNoSuchChoice) {
			t.Fatalf("deny label as approve = %v; want no such choice", err)
		}
		// An out-of-scope label is no link choice for any role.
		if err := dialog.WriteLinkAnswer(dir, id, "Always allow", dialog.RoleApprove, signedDigest(t, dir, id), "sha256:x", "c"); !errors.Is(err, dialog.ErrLinkNoSuchChoice) {
			t.Fatalf("session-scope label = %v; want no such choice", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "answers", id+".json")); !os.IsNotExist(err) {
			t.Fatalf("an answer was recorded: %v", err)
		}
	})
	t.Run("missing answers directory", func(t *testing.T) {
		dir := t.TempDir()
		id := openDialog(t, dir)
		if err := os.RemoveAll(filepath.Join(dir, "answers")); err != nil {
			t.Fatal(err)
		}
		if err := dialog.WriteLinkAnswer(dir, id, "Allow once", dialog.RoleApprove, signedDigest(t, dir, id), "sha256:x", "c"); !errors.Is(err, dialog.ErrLinkDialogGone) {
			t.Fatalf("missing answers dir = %v; want gone", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "answers")); !os.IsNotExist(err) {
			t.Fatalf("answers directory recreated: %v", err)
		}
	})
	t.Run("missing request file", func(t *testing.T) {
		dir := t.TempDir()
		if err := dialog.WriteLinkAnswer(dir, dialog.NewID(), "Allow once", dialog.RoleApprove, "sha256:none", "sha256:x", "c"); !errors.Is(err, dialog.ErrLinkDialogGone) {
			t.Fatalf("no dialog = %v; want gone", err)
		}
	})
	t.Run("resolved dialog", func(t *testing.T) {
		dir := t.TempDir()
		id := openDialog(t, dir)
		if ok, err := dialog.Resolve(dir, dialog.Resolved{ID: id, Outcome: dialog.OutcomeAllowed, Label: "Allow once", At: time.Now().UTC()}); err != nil || !ok {
			t.Fatalf("resolve: %v %v", ok, err)
		}
		if err := dialog.WriteLinkAnswer(dir, id, "Allow once", dialog.RoleApprove, signedDigest(t, dir, id), "sha256:x", "c"); !errors.Is(err, dialog.ErrLinkDialogGone) {
			t.Fatalf("resolved dialog = %v; want gone", err)
		}
	})
}

// The first answer wins: a second write, even of the other role, refuses
// with already answered and leaves the first answer's file exactly as it
// was.
func TestWriteLinkAnswerFirstAnswerWins(t *testing.T) {
	dir := t.TempDir()
	id := openDialog(t, dir)
	if err := dialog.WriteLinkAnswer(dir, id, "Allow once", dialog.RoleApprove, signedDigest(t, dir, id), "sha256:first", "cred-1"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "answers", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	err = dialog.WriteLinkAnswer(dir, id, "Deny", dialog.RoleDeny, signedDigest(t, dir, id), "sha256:second", "cred-2")
	if !errors.Is(err, dialog.ErrLinkAnswered) {
		t.Fatalf("second write = %v; want already answered", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "answers", id+".json"))
	if err != nil || string(raw) != string(after) {
		t.Fatalf("first answer changed: %s then %s (%v)", raw, after, err)
	}
}

// A failed directory sync returns its error — the write cannot promise
// durability — and may leave the answer file in place; the seam's fault
// injection drives it.
func TestWriteLinkAnswerSyncFaultReturnsInternalError(t *testing.T) {
	dir := t.TempDir()
	id := openDialog(t, dir)
	restore := fsq.SetPackageSyncDirFaultForTest(func(path string) error {
		if filepath.Base(path) == "answers" {
			return errors.New("sync failed")
		}
		return nil
	})
	defer restore()
	err := dialog.WriteLinkAnswer(dir, id, "Allow once", dialog.RoleApprove, signedDigest(t, dir, id), "sha256:x", "cred-1")
	if err == nil || errors.Is(err, dialog.ErrLinkDialogGone) || errors.Is(err, dialog.ErrLinkAnswered) || errors.Is(err, dialog.ErrLinkNoSuchChoice) {
		t.Fatalf("sync fault = %v; want a durability error", err)
	}
}

// No link is followed, at either seam: a dialog directory that is a link
// to elsewhere, and an answers directory that is a link, both refuse as
// gone and create nothing.
func TestWriteLinkAnswerFollowsNoLink(t *testing.T) {
	t.Run("dialog directory is a link", func(t *testing.T) {
		root := t.TempDir()
		real := filepath.Join(root, "elsewhere")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "b")
		if err := os.Symlink(real, dir); err != nil {
			t.Fatal(err)
		}
		if err := dialog.WriteLinkAnswer(dir, dialog.NewID(), "Allow once", dialog.RoleApprove, "sha256:none", "sha256:x", "c"); !errors.Is(err, dialog.ErrLinkDialogGone) {
			t.Fatalf("linked dialog dir = %v; want gone", err)
		}
		if _, err := os.ReadDir(real); err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(real); len(entries) != 0 {
			t.Fatalf("created inside the link target: %d entries", len(entries))
		}
	})
	t.Run("answers directory is a link", func(t *testing.T) {
		root := t.TempDir()
		elsewhere := filepath.Join(root, "elsewhere")
		if err := os.Mkdir(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "b")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, target := range map[string]string{"requests": "", "answers": elsewhere} {
			if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
				t.Fatal(err)
			}
			if target != "" {
				_ = os.Remove(filepath.Join(dir, name))
				if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
		}
		id := dialog.NewID()
		if err := dialog.WriteLinkAnswer(dir, id, "Allow once", dialog.RoleApprove, "sha256:none", "sha256:x", "c"); !errors.Is(err, dialog.ErrLinkDialogGone) {
			t.Fatalf("linked answers dir = %v; want gone", err)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Fatalf("created inside the link target: %d entries", len(entries))
		}
	})
}
