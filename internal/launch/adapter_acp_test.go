package launch

import (
	"context"
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
		Cwd: ticket.Cwd, AMQExecutable: ticket.AMQExecutable, ProviderExecutable: ticket.TargetArgv[0],
		TargetArgv: ticket.TargetArgv, Execution: plan.Execution,
	})
	if err != nil || prepared.State != ExecutionAcknowledged {
		t.Fatalf("prepare execution=%#v, %v; want acknowledged", prepared, err)
	}
}

// recordingProbe resolves amq-acp to path and records every execution.
type recordingProbe struct {
	path string
	ran  *[]string
}

func (probe recordingProbe) LookPath(string) (string, error) { return probe.path, nil }
func (probe recordingProbe) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	*probe.ran = append(*probe.ran, name)
	return []byte("0.104.0"), nil
}

// acpSeatFixture is a reconcile request for one acp seat whose amq, agent
// and (unless noSibling) amq-acp live in a bin directory outside the
// project.
func acpSeatFixture(t *testing.T, noSibling bool) (ReconcileRequest, *ACPAdapter, string) {
	t.Helper()
	bin := t.TempDir()
	names := []string{"amq", "amq-acp", "grok"}
	if noSibling {
		names = []string{"amq", "grok"}
	}
	for _, name := range names {
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
	return req, adapter, agent
}

// Review of #1142: with no amq-acp beside amq and PATH resolving to one in
// the project, the capability probe executed project code before the
// containment refusal and before any trust prompt.
func TestLaunchNeverProbesAProjectContainedACPCompanion(t *testing.T) {
	req, adapter, _ := acpSeatFixture(t, true)
	contained := filepath.Join(req.ProjectRoot, "bin", "amq-acp")
	if err := os.MkdirAll(filepath.Dir(contained), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contained, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var ran []string
	adapter.probe = recordingProbe{path: contained, ran: &ran}

	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Fatalf("launch executed %q before refusing the project-contained companion", ran)
	}
	if result.Plan != nil && len(result.Plan.Agents) != 0 {
		t.Fatalf("launch planned a seat that runs a project-contained companion: %#v", result.Plan)
	}
}

// Review of #1142: the ticket bound only amq-acp, so replacing the nested
// agent with a link to a project script after planning still acknowledged
// the seat, and the host would have started it unchecked.
func TestLaunchRefusesAnACPAgentReplacedAfterPlanning(t *testing.T) {
	req, _, agent := acpSeatFixture(t, false)
	result, err := Reconcile(req)
	if err != nil || result.Plan == nil || len(result.Plan.Agents) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	script := filepath.Join(req.ProjectRoot, "agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(agent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(script, agent); err != nil {
		t.Fatal(err)
	}

	ticket, err := LoadExecutionTicket(req.Root, "grok")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareExecution(req.Root, "grok", ticket.LaunchNonce, ExecutionEnvelope{
		Cwd: ticket.Cwd, AMQExecutable: ticket.AMQExecutable, ProviderExecutable: ticket.TargetArgv[0],
		TargetArgv: ticket.TargetArgv, Execution: result.Plan.Agents[0].Execution,
	})
	if err == nil || prepared.State == ExecutionAcknowledged {
		t.Fatalf("prepare execution=%#v, %v; want a refusal for the replaced agent", prepared.State, err)
	}
}
