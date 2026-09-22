package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain must point HOME/USERPROFILE (and the Windows drive-letter pair) at
// one fresh temp dir for the whole package run, and temp-directory selection
// (TMPDIR/TMP/TEMP) at a private dir inside it: with the operator's real home
// visible, ~/.amqrc leaked authority into root resolution and tests wrote
// through to the live ~/.agent-mail (issue #988, observed 2026-09-22 by the
// #846 round-7 verifier: TestSetup*/TestRunEnvJSON*/TestCoopExec*/
// TestEnvAndBareSend* failed identically on main and ~/.agent-mail was
// modified during the run). Reverting the HOME override fails the home
// assertions; reverting the temp-dir override fails the TempDir assertions
// (rev-853 P0: with a TMPDIR inside the real home, the cwd-ancestor walk
// escaped the fake home and resolved the live ~/.amqrc).
func TestTestHomeIsIsolatedFromRealHome(t *testing.T) {
	if testRealHome == "" {
		t.Fatal("TestMain did not record the real home; the seam is not in effect")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	homeInfo, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	realInfo, err := os.Stat(testRealHome)
	if err == nil && os.SameFile(homeInfo, realInfo) {
		t.Fatalf("os.UserHomeDir still resolves the real home %s during the package run", testRealHome)
	}
	if !homeInfo.IsDir() || homeInfo.Mode().Perm()&0o777 != 0o700 {
		t.Fatalf("isolated test home %s is not a 0700 directory", home)
	}
	homeResolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cliSecureTempRoot", "os.TempDir"} {
		var dir string
		if name == "cliSecureTempRoot" {
			dir = cliSecureTempRoot
		} else {
			dir = os.TempDir()
		}
		dirResolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(dirResolved, homeResolved+string(filepath.Separator)) {
			t.Fatalf("%s %s is not under the isolated test home %s", name, dirResolved, homeResolved)
		}
	}
}

// TestWalkCeilingStopsAncestorEscapes is the regression for the issue #988
// rev-853 P0 vector: a fixture re-points HOME at a sibling temp dir
// (t.Setenv("HOME", t.TempDir())) and chdirs to another sibling; the
// cwd-ancestor walks must then stop at the isolated home instead of climbing
// to the operator's real home and resolving the live ~/.amqrc / ~/.agent-mail
// (observed 2026-09-22: TestSetup*/TestRunEnvJSON*/TestCoopExec* failed on
// main and the run modified the live queue). A marker .amqrc placed at the
// boundary makes the check machine-independent: without the ceiling the walk
// loads the marker as project config; with it, the boundary is skipped
// exactly like a home stop.
func TestWalkCeilingStopsAncestorEscapes(t *testing.T) {
	if walkCeilingForTests == "" {
		t.Fatal("walk ceiling not set by TestMain")
	}
	boundary := filepath.Join(walkCeilingForTests, ".amqrc")
	if err := os.WriteFile(boundary, []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(boundary) })

	oldHome := os.Getenv("HOME")
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	siblingHome := t.TempDir() // sibling of the probe cwd, like the fixtures
	probeCWD := t.TempDir()
	if err := os.Setenv("HOME", siblingHome); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(probeCWD); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("HOME", oldHome)
		_ = os.Chdir(oldCWD)
		resetAmqrcCache()
	})
	resetAmqrcCache()

	result, err := findAndLoadAmqrc()
	if err == nil {
		t.Fatalf("walk loaded .amqrc %q at or above the isolated boundary; want no project config", result.Path)
	}
	if !errors.Is(err, errAmqrcNotFound) {
		t.Fatalf("findAndLoadAmqrc escaped the isolated boundary: %v", err)
	}
}
