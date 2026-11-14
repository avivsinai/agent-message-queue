package launch

import (
	"os"
	"strings"
	"testing"
)

// A managed Codex seat pinned with workspace-write must never widen its
// sandbox through a path in argv: the launcher adds one fixed token and AMQ
// fills it at exec time from each seat's own session root.
func TestCodexSeatWritableRootsTokenBecomesItsMailboxesPath(t *testing.T) {
	tool, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	stub := stubDir + "/codex"
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec /bin/sh -c ': codex stub'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--sandbox", "workspace-write",
		"-c", "sandbox_workspace_write.writable_roots=[\"__AMQ_MAILBOXES__\"]",
	}
	root1 := t.TempDir()
	root2 := t.TempDir()
	resolved1, err := resolvedPath(root1)
	if err != nil {
		t.Fatal(err)
	}
	resolved2, err := resolvedPath(root2)
	if err != nil {
		t.Fatal(err)
	}
	session := t.TempDir()
	base1 := PlanRequest{Handle: "s", LaunchNonce: "96969696-9696-4969-8969-969696969696", CommittedArgs: args,
		ProjectRoot: session, SessionRoot: root1, Cwd: session, AMQExecutable: tool, ResumePolicy: ResumeEnabled}
	base2 := base1
	base2.SessionRoot = root2
	a1, err := NewCodexAdapter(stub).PlanFresh(base1)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := NewCodexAdapter(stub).PlanFresh(base2)
	if err != nil {
		t.Fatal(err)
	}
	slotsOf := func(plan AgentPlan) []DynamicArg {
		var out []DynamicArg
		for _, d := range plan.DynamicArgv {
			if d.Kind == DynamicArgCodexMailboxesPath {
				out = append(out, d)
			}
		}
		return out
	}
	slots1, slots2 := slotsOf(a1), slotsOf(a2)
	if len(slots1) != 1 || len(slots2) != 1 {
		t.Fatalf("mailbox slots: a1=%d a2=%d; want 1 each", len(slots1), len(slots2))
	}
	si1, si2 := slots1[0].Index, slots2[0].Index
	if a1.Argv[si1] != codexMailboxesArgPrefix+codexMailboxesToken {
		t.Fatalf("a1 slot argv=%q; want the un-filled token", a1.Argv[si1])
	}
	if a2.Argv[si2] != codexMailboxesArgPrefix+codexMailboxesToken {
		t.Fatalf("a2 slot argv=%q; want the un-filled token", a2.Argv[si2])
	}
	// The plan argv holds the token; the trust projection binds the kind, not
	// the path (the path arrives at exec only through ticket.SessionRoot).
	planToExecute := func(plan AgentPlan, seatRoot string) []string {
		ticket, err := NewExecutionTicket(ExecutionTicketRequest{Handle: plan.Handle, LaunchNonce: "96969696-9696-4969-8969-969696969696",
			Mode: plan.AdapterMode, Provider: CodexProvider, ProviderVersion: "0.167.0",
			PreSpawnAcquire: false, Backend: CommandsBackendName, Profile: CommandsProfile().Identity(),
			ProjectRoot: session, SessionRoot: seatRoot, Cwd: session,
			ProviderExecutable: plan.Argv[0], AMQExecutable: tool,
			TargetArgv: plan.Argv, DynamicArgv: plan.DynamicArgv})
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := ResolveExecutionArgv(ticket, []string{"AM_ME=s", "AM_ROOT=" + seatRoot, "AM_BASE_ROOT=" + seatRoot, "AM_SESSION=", "AM_ROOT_ID=", "AM_BASE_ROOT_ID="})
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	out1 := planToExecute(a1, root1)
	out2 := planToExecute(a2, root2)
	want1 := codexMailboxesArgPrefix + "[" + tomlBasicString(resolved1+"/agents") + "]"
	want2 := codexMailboxesArgPrefix + "[" + tomlBasicString(resolved2+"/agents") + "]"
	if out1[si1] != want1 {
		t.Errorf("slot for seat 1 resolved to %q; want %q", out1[si1], want1)
	}
	if out2[si2] != want2 {
		t.Errorf("slot for seat 2 resolved to %q; want %q", out2[si2], want2)
	}
	if strings.Contains(out1[si1], "__AMQ_MAILBOXES__") {
		t.Error("token survived past the fill")
	}
	if strings.Contains(a1.Argv[si1], resolved1) {
		t.Error("seat 1's path leaked into plan argv (should only fill at exec)")
	}
}
