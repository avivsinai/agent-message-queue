package launch

import (
	"os"
	"slices"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// RunConformance exercises declared capabilities identically for any backend
// and requires stable unsupported / unknown for the rest. The Inspect-unknown
// injection and typo-refusal cases always run so tmux/cmux/ghostty can reuse
// them unchanged.
func RunConformance(t *testing.T, b Backend) {
	t.Helper()
	detect := b.Detect()
	if !detect.Available {
		t.Fatal("Detect reported unavailable")
	}
	if err := detect.Validate(); err != nil {
		t.Fatalf("Detect profile: %v", err)
	}
	profile := detect.Profile
	_, root := harnessRoot(t)

	t.Run("declared_capabilities", func(t *testing.T) {
		for _, cap := range profile.Capabilities {
			effective := slices.Contains(detect.Effective, cap)
			t.Run(string(cap), func(t *testing.T) {
				if !effective {
					assertDegradedOrUnsupported(t, b, cap, detect, root)
					return
				}
				switch cap {
				case CapPlanOnly:
					assertPlanOnlyCreate(t, b, root)
				case CapCreate, CapInspect, CapClose, CapFocus, CapReclaim:
					t.Fatal("managed capability declared but this skeleton has no managed assertion yet")
				default:
					t.Fatalf("unknown declared capability %q", cap)
				}
			})
		}
	})

	t.Run("undeclared_create_is_not_managed_success", func(t *testing.T) {
		if profile.Has(CapCreate) {
			t.Skip("managed create is declared")
		}
		result, err := b.Create(CreateRequest{Session: "collab", Plan: harnessPlan(), Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome == OutcomeCreated {
			t.Fatalf("undeclared create invented managed success: %#v", result)
		}
		assertNoBinding(t, root)
	})

	t.Run("undeclared_inspect_is_unknown", func(t *testing.T) {
		if profile.Has(CapInspect) {
			t.Skip("inspect is declared")
		}
		got, err := b.Inspect(InspectRequest{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != InspectUnknown || got.Evidence == "" {
			t.Fatalf("undeclared Inspect = %#v, want unknown with evidence", got)
		}
		assertNoBinding(t, root)
	})

	t.Run("undeclared_close_is_unsupported", func(t *testing.T) {
		if profile.Has(CapClose) {
			t.Skip("close is declared")
		}
		got, err := b.Close(CloseRequest{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != OutcomeUnsupported {
			t.Fatalf("undeclared Close = %#v, want unsupported", got)
		}
		assertNoBinding(t, root)
	})

	t.Run("inspect_unknown_zero_mutations", func(t *testing.T) {
		creates, closes := 0, 0
		injected := unknownInspect{Backend: countingBackend{Backend: b, creates: &creates, closes: &closes}}
		got, err := injected.Inspect(InspectRequest{Root: root, Binding: harnessBinding()})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != InspectUnknown {
			t.Fatalf("injected Inspect = %#v, want unknown", got)
		}
		if creates != 0 || closes != 0 {
			t.Fatalf("Inspect unknown mutated backend state: creates=%d closes=%d", creates, closes)
		}
		assertNoBinding(t, root)
	})

	t.Run("typo_refusal", func(t *testing.T) {
		// A missing session name must not be created by the backend. plan_only
		// emits commands; managed backends reuse this to refuse layout create.
		dir := t.TempDir()
		identity, err := fsq.SnapshotDeliveryRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		typoRoot, err := fsq.OpenDeliveryRoot(dir, identity)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = typoRoot.Close() })
		before, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		result, err := b.Create(CreateRequest{Session: "no-such-session", Plan: harnessPlan(), Root: typoRoot})
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome == OutcomeCreated {
			t.Fatal("typo session invented managed create success")
		}
		assertNoBinding(t, typoRoot)
		after, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := names(after), names(before); !slices.Equal(got, want) {
			t.Fatalf("backend wrote files for a missing session: before=%v after=%v", want, got)
		}
	})

	t.Run("stale_binding_recovery", func(t *testing.T) {
		if !profile.Has(CapInspect) || !profile.Has(CapCreate) {
			t.Skip("managed inspect/create not in profile; reused by tmux/cmux/ghostty")
		}
	})

	t.Run("duplicate_spawn_prevention", func(t *testing.T) {
		if !profile.Has(CapCreate) {
			t.Skip("managed create not in profile; reused by tmux/cmux/ghostty")
		}
	})

	t.Run("foreign_context_binding_rejection", func(t *testing.T) {
		if !profile.Has(CapInspect) {
			t.Skip("inspect not in profile; reused by tmux/cmux/ghostty")
		}
	})

	t.Run("capability_degradation", func(t *testing.T) {
		if len(detect.Degradations) == 0 {
			t.Skip("no runtime degradation on this profile")
		}
		for _, deg := range detect.Degradations {
			if deg.Reason == "" {
				t.Fatalf("degradation %q has no reason", deg.Capability)
			}
			if slices.Contains(detect.Effective, deg.Capability) {
				t.Fatalf("degraded capability %q remains effective", deg.Capability)
			}
		}
	})
}

func assertPlanOnlyCreate(t *testing.T, b Backend, root *fsq.DeliveryRoot) {
	t.Helper()
	plan := harnessPlan()
	result, err := b.Create(CreateRequest{Session: "collab", Plan: plan, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeCommandsEmitted || !result.ActionRequired {
		t.Fatalf("plan_only Create = %#v, want commands_emitted + action_required", result)
	}
	if _, err := DecodePlan(result.Plan); err != nil {
		t.Fatal(err)
	}
	if len(result.Commands) != len(plan.Agents) {
		t.Fatalf("emitted %d commands, want %d", len(result.Commands), len(plan.Agents))
	}
	assertNoBinding(t, root)
}

func assertDegradedOrUnsupported(t *testing.T, b Backend, cap Capability, detect DetectResult, root *fsq.DeliveryRoot) {
	t.Helper()
	found := false
	for _, deg := range detect.Degradations {
		if deg.Capability == cap && deg.Reason != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("capability %q is not effective and has no degradation reason", cap)
	}
	switch cap {
	case CapClose:
		got, err := b.Close(CloseRequest{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != OutcomeUnsupported {
			t.Fatalf("degraded Close = %#v, want unsupported", got)
		}
	case CapInspect:
		got, err := b.Inspect(InspectRequest{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == InspectPresent {
			t.Fatalf("degraded Inspect invented present: %#v", got)
		}
	}
}

func assertNoBinding(t *testing.T, root *fsq.DeliveryRoot) {
	t.Helper()
	if root == nil {
		return
	}
	if _, err := LoadBinding(root); err == nil {
		t.Fatal("backend wrote a binding")
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}

func harnessRoot(t *testing.T) (string, *fsq.DeliveryRoot) {
	t.Helper()
	dir := t.TempDir()
	identity, err := fsq.SnapshotDeliveryRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

func harnessPlan() Plan {
	return Plan{Version: PlanVersion, Agents: []AgentPlan{
		{
			Handle: "claude", Argv: []string{"/usr/local/bin/claude", "--session-id", "claude:one"},
			EnvOverlay: map[string]string{"LANG": "C"}, Cwd: "/work/project",
			AdapterMode: AdapterModeMint, ResumePolicy: ResumeEnabled,
			LaunchNonce: "launch-one", ConversationID: "claude:one",
			DynamicArgv: []DynamicArg{{Index: 2, Kind: DynamicArgConversationID}},
		},
		{
			Handle: "codex", Argv: []string{"/usr/local/bin/codex", "--launch-nonce", "launch-one"}, Cwd: "/work/project",
			AdapterMode: AdapterModeCapture, ResumePolicy: ResumeFresh,
			LaunchNonce: "launch-one",
			DynamicArgv: []DynamicArg{{Index: 2, Kind: DynamicArgLaunchNonce}},
		},
	}}
}

func harnessBinding() BindingRecord {
	return BindingRecord{
		Version: BindingVersion, Backend: "tmux", HostIdentity: "host:one",
		InstanceIdentity: "tmux-server:one", Profile: "tmux/darwin/v1", LaunchNonce: "nonce-one",
		Resources: ResourceIdentitySet{Version: ResourceSetVersion},
	}
}

type countingBackend struct {
	Backend
	creates *int
	closes  *int
}

func (b countingBackend) Create(req CreateRequest) (CreateResult, error) {
	*b.creates++
	return b.Backend.Create(req)
}

func (b countingBackend) Close(req CloseRequest) (CloseResult, error) {
	*b.closes++
	return b.Backend.Close(req)
}

type unknownInspect struct{ Backend }

func (unknownInspect) Inspect(InspectRequest) (InspectResult, error) {
	return InspectResult{
		Status:         InspectUnknown,
		Evidence:       "injected unknown",
		ActionRequired: true,
	}, nil
}
