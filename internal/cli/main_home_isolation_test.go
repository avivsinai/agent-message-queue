package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain must point HOME/USERPROFILE (and the Windows drive-letter pair) at
// one fresh temp dir for the whole package run: with the operator's real home
// visible, ~/.amqrc leaked authority into root resolution and tests wrote
// through to the live ~/.agent-mail (issue #988, observed 2026-09-22 by the
// #846 round-7 verifier: TestSetup*/TestRunEnvJSON*/TestCoopExec*/
// TestEnvAndBareSend* failed identically on main and ~/.agent-mail was
// modified during the run). Reverting the override fails this everywhere.
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
	root, err := filepath.EvalSymlinks(cliSecureTempRoot)
	if err != nil {
		t.Fatal(err)
	}
	homeResolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(root, homeResolved+string(filepath.Separator)) {
		t.Fatalf("secure temp root %s is not under the isolated test home %s", root, homeResolved)
	}
}
