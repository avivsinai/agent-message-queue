//go:build darwin || linux

package cli

import (
	"errors"
	"slices"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestManagedLaunchReexecPreservesPIDAndTargetArgv(t *testing.T) {
	sentinel := errors.New("exec sentinel")
	old := coopExecProcess
	t.Cleanup(func() { coopExecProcess = old })
	var gotPath string
	var gotArgv, gotEnv []string
	coopExecProcess = func(path string, argv, env []string) error {
		gotPath = path
		gotArgv = slices.Clone(argv)
		gotEnv = slices.Clone(env)
		return sentinel
	}
	target := []string{"/opt/provider", "--resume", "conversation"}
	env := []string{"AM_ROOT=/queue", "TOKEN=value"}
	options := &launch.PrepareExecutionOptions{WakeMode: "enabled", RequireWake: true}
	err := reexecManagedLaunchWrapper("/queue", "codex", "11111111-1111-4111-8111-111111111111", target[0], target, env, options)
	if !errors.Is(err, sentinel) {
		t.Fatalf("reexec error = %v, want sentinel", err)
	}
	if gotPath == "" || len(gotArgv) < 12 || gotArgv[1] != "__launch-exec" {
		t.Fatalf("wrapper exec = path %q argv %#v", gotPath, gotArgv)
	}
	dash := slices.Index(gotArgv, "--")
	if dash < 0 || !slices.Equal(gotArgv[dash+1:], target) {
		t.Fatalf("wrapper target tail = %#v, want %#v", gotArgv[dash+1:], target)
	}
	optionsAt := slices.Index(gotArgv, "--"+managedExecutionOptionsFlag)
	if optionsAt < 0 || optionsAt+1 >= dash {
		t.Fatalf("private wrapper omitted execution options: %#v", gotArgv)
	}
	decoded, err := decodeManagedExecutionOptions(gotArgv[optionsAt+1])
	if err != nil || !slices.Equal(decoded.InjectorArgs, options.InjectorArgs) || decoded.RequireWake != options.RequireWake || decoded.WakeMode != options.WakeMode {
		t.Fatalf("private wrapper options = %#v, %v", decoded, err)
	}
	if !slices.Equal(gotEnv, env) {
		t.Fatalf("wrapper env = %#v, want %#v", gotEnv, env)
	}
}
