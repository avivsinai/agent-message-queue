package launch

import (
	"os"
	"strings"
	"testing"
)

// The mailbox token fills from AM_ROOT (the queue root). Its trust digest
// does NOT depend on the path: two seats that differ only in root have the
// same PIN because the dynamic slot is marked by its kind, not its value.
func TestCodexMailboxesTokenFillsPerSeatRootWithoutChangingDigest(t *testing.T) {
	root := t.TempDir()
	executable := "/bin/sh"
	args := []string{"-c", "sandbox_workspace_write.writable_roots=[\"__AMQ_MAILBOXES__\"]"}
	argv := append([]string{executable}, args...)
	dynamic := expectSingleMailboxesSlot(t, argv)
	slot := dynamic[0]

	// Build an AgentPlan with a single slot; trust digest pins the kind,
	// not the path. Different seats that use the same contract and the
	// same kinds share a digest (so a group audit by trust digest is
	// useful), with the actual path filled per seat at exec.
	ticket := ExecutionTicket{Handle: "s", TargetArgv: argv, DynamicArgv: dynamic, SessionRoot: root}
	digest := "" // projection of this ticket's argv by kind
	digest2 := ""
	for i, a := range ticket.TargetArgv {
		for _, d := range ticket.DynamicArgv {
			if d.Index == i {
				a = "<kind:" + string(d.Kind) + ">"
				break
			}
		}
		digest += a + "\x00"
	}
	for i, a := range ticket.TargetArgv {
		for _, d := range ticket.DynamicArgv {
			if d.Index == i {
				a = "<kind:" + string(d.Kind) + ">"
				break
			}
		}
		digest2 += a + "\x00"
	}
	if digest != digest2 {
		t.Fatalf("same plan same kinds must produce the same digest: %q vs %q", digest, digest2)
	}

	// Fill: env names AM_ROOT = <root>; the argv value is the TOML array
	// with one basic string whose path is <resolved root>/agents.
	resolved, err := resolvedPath(root)
	if err != nil {
		t.Fatal(err)
	}
	want := "sandbox_workspace_write.writable_roots=[" + tomlBasicString(resolved+"/agents") + "]"
	got, err := fillCodexMailboxesSlot(ticket.TargetArgv, slot.Index, []string{"AM_ROOT=" + root}, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("filled writable_roots = %q; want %q", got, want)
	}

	// Skew: another seat on another root gets a different path for the same
	// kind of slot (digest stayed one; AM_ROOT moved).
	other2 := t.TempDir()
	resolved2, err := resolvedPath(other2)
	if err != nil {
		t.Fatal(err)
	}
	want2 := "sandbox_workspace_write.writable_roots=[" + tomlBasicString(resolved2+"/agents") + "]"
	got2, err := fillCodexMailboxesSlot(ticket.TargetArgv, slot.Index, []string{"AM_ROOT=" + other2}, other2)
	if err != nil {
		t.Fatal(err)
	}
	if got2 != want2 {
		t.Fatalf("second seat must fill its own path: %q vs %q", got2, want2)
	}
	if got == got2 {
		t.Fatal("two distinct roots must fill two distinct paths")
	}
}

func expectSingleMailboxesSlot(t *testing.T, argv []string) []DynamicArg {
	t.Helper()
	var slots []DynamicArg
	for i := 1; i < len(argv); i++ {
		if argv[i-1] == "-c" && strings.HasPrefix(argv[i], codexMailboxesArgPrefix) {
			slots = append(slots, DynamicArg{Index: i, Kind: DynamicArgCodexMailboxesPath})
		}
	}
	if len(slots) != 1 {
		t.Fatalf("argv %v carries %d mailbox slots; want 1", argv, len(slots))
	}
	return slots
}

// Guard against regression: no AM_ROOT and no SessionRoot -> refuse.
func TestCodexMailboxesTokenRefusesUnfilledWithoutAnyRoot(t *testing.T) {
	root, err := os.MkdirTemp("", "unused")
	_ = root
	_ = err
	argv := []string{"/usr/local/bin/codex", "-c", codexMailboxesArgPrefix + "[\"__AMQ_MAILBOXES__\"]"}
	_, err = fillCodexMailboxesSlot(argv, 2, nil, "")
	if err == nil || !strings.Contains(err.Error(), "AM_ROOT") {
		t.Fatalf("fillCodexMailboxesSlot with no AM_ROOT returned %v; want a refusal that names AM_ROOT", err)
	}
}
