package cli

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestManagedExecutionOptionsCodecRoundTripAndHostileInput(t *testing.T) {
	want := launch.PrepareExecutionOptions{
		RequireWake: true, NoGitignore: true, Named: true, WakeMode: "enabled",
		InjectorMode: "paste", InjectorVia: "/opt/amq/injector", InjectorArgs: []string{"--fixed", "value"},
		SymphonyEvents: []string{"after_create", "before_run"}, SymphonyWorkspaceKey: "workspace-7",
	}
	encoded, err := encodeManagedExecutionOptions(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeManagedExecutionOptions(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}

	unknown := base64.RawURLEncoding.EncodeToString([]byte(`{"wake_mode":"enabled","arbitrary_hook":"/tmp/run"}`))
	if _, err := decodeManagedExecutionOptions(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("arbitrary hook decode error = %v", err)
	}
	invalid := want
	invalid.SymphonyEvents = []string{"unknown"}
	if _, err := encodeManagedExecutionOptions(invalid); err == nil || !strings.Contains(err.Error(), "unknown symphony event") {
		t.Fatalf("unknown event error = %v", err)
	}
	argsWithoutVia := want
	argsWithoutVia.InjectorVia = ""
	if _, err := encodeManagedExecutionOptions(argsWithoutVia); err == nil || !strings.Contains(err.Error(), "injector args require via") {
		t.Fatalf("injector args without via error = %v", err)
	}
}

func TestCoopExecRejectsManagedOptionsWithoutLaunchTicket(t *testing.T) {
	t.Setenv(launch.InternalLaunchNonceEnv, "")
	err := runCoopExec([]string{"--managed-symphony-event", "after_create", "true"})
	if err == nil || GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "require a trusted launch ticket") {
		t.Fatalf("untrusted managed option error = %v", err)
	}
}
