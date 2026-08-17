package launchapi_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExternalConsumerCompileAndNegotiateV1(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve launchapi test source")
	}
	repositoryRoot := filepath.Dir(filepath.Dir(sourceFile))
	consumer := t.TempDir()
	goMod := fmt.Sprintf(`module example.com/amq-launch-consumer

go 1.25.0

require github.com/avivsinai/agent-message-queue v0.0.0

replace github.com/avivsinai/agent-message-queue => %s
`, filepath.ToSlash(repositoryRoot))
	consumerTest := `package consumer

import (
	"context"
	"testing"

    "github.com/avivsinai/agent-message-queue/launchapi"
)

func TestContract(t *testing.T) {
	var prepare func(context.Context, launchapi.PrepareRequestV1) (launchapi.PrepareResultV1, error) = launchapi.Prepare
	_ = prepare
	var apply func(context.Context, launchapi.ApplyRequestV1) (launchapi.ApplyResultV1, error) = launchapi.Apply
	_ = apply
	var inspect func(context.Context, launchapi.InspectRequestV1) (launchapi.InspectResultV1, error) = launchapi.Inspect
	var focus func(context.Context, launchapi.FocusRequestV1) (launchapi.FocusResultV1, error) = launchapi.Focus
	var closeLaunch func(context.Context, launchapi.CloseRequestV1) (launchapi.CloseResultV1, error) = launchapi.Close
	_, _, _ = inspect, focus, closeLaunch
	var action launchapi.RequiredActionKindV1 = launchapi.RequiredActionTrustConfirmation
	var choice launchapi.DecisionChoiceV1 = launchapi.DecisionTrustExactSubject
	if action == "" || choice == "" {
		t.Fatal("typed Prepare action contract is empty")
	}
    intent := launchapi.LaunchIntentV1{
        IntentVersion: launchapi.IntentVersionV1,
        Participants: []launchapi.ParticipantV1{{Handle: "operator", Runnable: false}},
    }
    if err := intent.Validate(); err != nil {
        t.Fatal(err)
    }
    negotiated, err := launchapi.Negotiate(launchapi.RequirementV1{
        ContractSemver: ">=0.61.0 <0.62.0",
        IntentVersion: 1,
        ResultVersion: 1,
        Features: []string{"launch_intent_v1"},
    })
    if err != nil || negotiated.ContractSemver != "0.61.0" {
        t.Fatalf("negotiated=%#v err=%v", negotiated, err)
    }
}
`
	if err := os.WriteFile(filepath.Join(consumer, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumer, "contract_test.go"), []byte(consumerTest), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-mod=mod", "./...")
	command.Dir = consumer
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("external consumer compile: %v\n%s", err, output)
	}
}
