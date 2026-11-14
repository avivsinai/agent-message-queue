package launch

import (
	"os"
	"strings"
	"testing"
)

// canonicalMailboxesSlot projects the argument at slot index i by the same rule
// plan.go uses to build the trust digest. The kind sits in Dynamic; the Value
// is cleared, mirroring the canonicalArgument{Dynamic: dynamic.Kind} assignment.
func canonicalMailboxesSlot(plan AgentPlan, i int) canonicalArgument {
	for _, d := range plan.DynamicArgv {
		if d.Index == i {
			return canonicalArgument{Dynamic: d.Kind}
		}
	}
	return canonicalArgument{Value: plan.Argv[i]}
}

// A managed Codex seat pinned with workspace-write must never widen its
// sandbox through a path in argv: the launcher adds one fixed token and AMQ
// fills it at exec time from each seat's own session root. Two seats that
// differ only in root share the mailbox-slot contribution to the trust
// projection (kind codex_mailboxes_path), with no path leaking in.
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
	// The mailbox slot is bound by its kind (create-only invariant of the
	// trust projection in plan.go), not its path. Compute each seat's
	// canonicalArgument at its slot index and assert both collapse to the
	// same kind (codex_mailboxes_path), with the path not present in Value.
	c1 := canonicalMailboxesSlot(a1, si1)
	c2 := canonicalMailboxesSlot(a2, si2)
	if c1.Dynamic != DynamicArgCodexMailboxesPath || c2.Dynamic != DynamicArgCodexMailboxesPath {
		t.Fatalf("slot canonicalization lost the kind: c1=%+v c2=%+v", c1, c2)
	}
	if c1.Value != "" || c2.Value != "" {
		t.Fatalf("trust projection included the path: c1=%q c2=%q", c1.Value, c2.Value)
	}
	// Execute: run through the full ticket pipeline so every trust-template
	// check fires before the fill.
	planToExecute := func(plan AgentPlan, seatRoot string) []string {
		ticket, err := NewExecutionTicket(ExecutionTicketRequest{Handle: plan.Handle, LaunchNonce: "96969696-9696-4969-8969-969696969696",
			Mode: plan.AdapterMode, Provider: CodexProvider, ProviderVersion: "0.167.0",
			PreSpawnAcquire: false,
			Backend: CommandsBackendName, Profile: CommandsProfile().Identity(),
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
