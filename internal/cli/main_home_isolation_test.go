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
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(home, os.TempDir()) {
		t.Fatalf("test home %s is not inside the temp tree; the real home leaked into the package run", home)
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
	// The isolated home is fresh at package start and deleted after m.Run();
	// whether any test created state inside it is that test's business — the
	// canary run in CI proves nothing outside the isolated home is touched.
}
