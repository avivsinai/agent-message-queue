package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

type countingLaunchBackend struct {
	launch.Backend
	creates *int
	closes  *int
}

func (b countingLaunchBackend) Create(req launch.CreateRequest) (launch.CreateResult, error) {
	*b.creates++
	return b.Backend.Create(req)
}

func (b countingLaunchBackend) Close(req launch.CloseRequest) (launch.CloseResult, error) {
	*b.closes++
	return b.Backend.Close(req)
}

func TestLaunchPathDeclaredPlanOnlyEmitsCoopExec(t *testing.T) {
	project, _ := launchCLIFixture(t, "collab")
	launchIsTerminal = func() bool { return true }
	launchInput = func() *bufio.Reader { return bufio.NewReader(strings.NewReader("y\n")) }

	stdout, _, err := captureEnvOutput(t, func() error { return runLaunch([]string{}) })
	if GetExitCode(err) != ExitActionRequired {
		t.Fatalf("exit=%d err=%v output=%s", GetExitCode(err), err, stdout)
	}
	if !strings.Contains(stdout, "coop exec") {
		t.Fatalf("plan_only launch did not emit coop exec: %s", stdout)
	}
	assertNoLaunchBinding(t, filepath.Join(project, defaultCoopRoot, "collab"))
}

func assertNoLaunchBinding(t *testing.T, sessionRoot string) {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(sessionRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := launch.LoadBinding(root); err == nil {
		t.Fatal("plan_only launch wrote a binding")
	}
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		names = append(names, rel+"\t"+info.Mode().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}
