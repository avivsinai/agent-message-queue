package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
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

func TestManagedExecutionOptionsDisabledWakeRequiresReason(t *testing.T) {
	if err := validateManagedExecutionOptions(launch.PrepareExecutionOptions{WakeMode: "disabled"}); err == nil {
		t.Fatal("disabled wake without an audit reason succeeded")
	}
	if err := validateManagedExecutionOptions(launch.PrepareExecutionOptions{WakeMode: "disabled", AuditReason: "operator policy", RequireWake: true}); err == nil {
		t.Fatal("disabled wake with require_wake succeeded")
	}
}

func TestCoopExecRejectsManagedOptionsWithoutLaunchTicket(t *testing.T) {
	t.Setenv(launch.InternalLaunchNonceEnv, "")
	err := runCoopExec([]string{"--managed-symphony-event", "after_create", "true"})
	if err == nil || GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "require a trusted launch ticket") {
		t.Fatalf("untrusted managed option error = %v", err)
	}
}

func TestManagedCoopExecRequiresExplicitAbsoluteRoot(t *testing.T) {
	t.Setenv(launch.InternalLaunchNonceEnv, "11111111-1111-4111-8111-111111111111")
	for _, args := range [][]string{
		{"--managed-symphony-event", "after_create", "true"},
		{"--root", "relative/root", "--managed-symphony-event", "after_create", "true"},
	} {
		err := runCoopExec(args)
		if err == nil || GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), "explicit absolute --root") {
			t.Fatalf("managed exact-root error for %#v = %v", args, err)
		}
	}
}

func TestManagedCoopExecDoesNotProvisionMissingTicketRoot(t *testing.T) {
	t.Setenv(launch.InternalLaunchNonceEnv, "11111111-1111-4111-8111-111111111111")
	missing := filepath.Join(t.TempDir(), "missing")
	err := runCoopExec([]string{
		"--root", missing, "--no-wake", "--managed-no-wake-reason", "test",
		"--me", "codex", "true",
	})
	if err == nil || GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), "before root mutation") {
		t.Fatalf("missing managed root error = %v", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("managed refusal provisioned %s: %v", missing, statErr)
	}
}

func TestPrivateLaunchWrapperRejectsInvalidExecutionOptionsBeforeRootAccess(t *testing.T) {
	invalid := base64.RawURLEncoding.EncodeToString([]byte(`{"wake_mode":"enabled","arbitrary_hook":"/tmp/run"}`))
	err := runLaunchExec([]string{
		"--root", "/definitely/missing", "--handle", "codex",
		"--nonce", "11111111-1111-4111-8111-111111111111", "--target", "/bin/true",
		"--" + managedExecutionOptionsFlag, invalid, "--", "/bin/true",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid managed execution options") {
		t.Fatalf("private wrapper error = %v", err)
	}
}
