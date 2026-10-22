package launch

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLaunchPlansAnACPSeatAsAHostWithWakeDisabled(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"amq", "amq-acp", "grok"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	req := reconcileFixture(t, Commands{})
	agent := filepath.Join(bin, "grok")
	req.Config.Agents = []ProjectAgentConfig{{Handle: "grok", Adapter: ACPProvider, Command: []string{agent, "agent", "stdio"}, ResumePolicy: ResumeFresh}}
	adapter := NewACPAdapter(filepath.Join(bin, "amq"))
	adapter.probe = fakeCommandProbe{outputs: map[string]string{"--version": "0.104.0"}}
	req.Adapters = map[string]HarnessAdapter{ACPProvider: adapter}
	req.AMQPath = filepath.Join(bin, "amq")

	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan == nil || len(result.Plan.Agents) != 1 || len(result.Commands) != 1 {
		t.Fatalf("result=%#v", result)
	}
	resolvedBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.Plan.Agents[0]
	if want := []string{filepath.Join(resolvedBin, "amq-acp"), "host", "--", agent, "agent", "stdio"}; !slices.Equal(plan.Argv, want) {
		t.Fatalf("argv=%q; want %q", plan.Argv, want)
	}
	if plan.AdapterMode != AdapterModeUnsupported || plan.Execution == nil || plan.Execution.WakeMode != "disabled" || plan.Execution.AuditReason != acpNoWakeReason {
		t.Fatalf("plan=%#v execution=%#v", plan, plan.Execution)
	}
	if argv := result.Commands[0].Argv; !slices.Contains(argv, "--no-wake") || !slices.Contains(argv, acpNoWakeReason) {
		t.Fatalf("emitted command=%q; want coop exec --no-wake with the audit reason", argv)
	}

	// The emitted coop exec acknowledges the seat's start.
	ticket, err := LoadExecutionTicket(req.Root, "grok")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareExecution(req.Root, "grok", ticket.LaunchNonce, ExecutionEnvelope{
		Cwd: ticket.Cwd, AMQExecutable: ticket.AMQExecutable, ProviderExecutable: ticket.ProviderExecutable,
		TargetArgv: ticket.TargetArgv, Execution: plan.Execution,
	})
	if err != nil || prepared.State != ExecutionAcknowledged {
		t.Fatalf("prepare execution=%#v, %v; want acknowledged", prepared, err)
	}
}
