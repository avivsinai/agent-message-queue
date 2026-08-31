package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestWakeRemedyQuotesEveryArgumentAndPreservesActionArgv(t *testing.T) {
	root := "/tmp/amq root/it's"
	agent := "codex;echo unsafe"
	remedy := wakeRecoverOwnerRemedy(root, agent)

	want := "amq wake recover-owner --root '/tmp/amq root/it'\"'\"'s' --me 'codex;echo unsafe'"
	if got := remedy.String(); got != want {
		t.Fatalf("rendered remedy = %q, want %q", got, want)
	}
	if !strings.Contains(remedy.String(), "--root '/tmp/amq root/it'") {
		t.Fatalf("root was not shell-quoted: %q", remedy.String())
	}

	oldExecutable := wakeCheckExecutable
	wakeCheckExecutable = func() (string, error) { return "/usr/local/bin/amq", nil }
	t.Cleanup(func() { wakeCheckExecutable = oldExecutable })
	command := remedy.actionCommand()
	if command == nil || command.Program != "/usr/local/bin/amq" {
		t.Fatalf("action command = %#v", command)
	}
	wantArgs := []string{"wake", "recover-owner", "--root", root, "--me", agent}
	if !reflect.DeepEqual(command.Args, wantArgs) {
		t.Fatalf("action args = %#v, want %#v", command.Args, wantArgs)
	}
}

func TestWakeRemedyActionCommandRejectsNonAMQArgv(t *testing.T) {
	if command := newWakeRemedy("doctor", "--root", "/queue").actionCommand(); command != nil {
		t.Fatalf("action command = %#v, want nil", command)
	}
}
