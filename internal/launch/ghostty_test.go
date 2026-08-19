package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type fakeGhostty struct {
	mu        sync.Mutex
	log       [][]string
	version   string
	windows   []fakeGhosttyWindow
	seq       int
	fail      string
	unhealthy bool
	reuseNext string
}

type fakeGhosttyWindow struct {
	id        string
	tabID     string
	terminals []string
}

func (f *fakeGhostty) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) == 0 {
		return "", errors.New("missing ghostty op")
	}
	f.log = append(f.log, append([]string(nil), args...))
	op := args[0]
	if f.fail == op || f.fail == "*" {
		return "", errors.New("injected failure")
	}
	switch op {
	case "version":
		return f.version, nil
	case "new-window":
		f.seq++
		windowID := f.reuseNext
		if windowID == "" {
			windowID = fmt.Sprintf("tab-group-%d", f.seq)
		}
		f.reuseNext = ""
		tabID := fmt.Sprintf("tab-%d", f.seq)
		term := f.nextUUID()
		f.windows = append(f.windows, fakeGhosttyWindow{id: windowID, tabID: tabID, terminals: []string{term}})
		return windowID + "|" + tabID + "|" + term, nil
	case "split":
		parent := args[1]
		for i := range f.windows {
			for _, id := range f.windows[i].terminals {
				if id == strings.ToUpper(parent) || id == parent {
					term := f.nextUUID()
					f.windows[i].terminals = append(f.windows[i].terminals, term)
					return term, nil
				}
			}
		}
		return "", errors.New("ghostty terminal not unique")
	case "list-windows":
		ids := make([]string, 0, len(f.windows))
		for _, window := range f.windows {
			ids = append(ids, window.id)
		}
		return strings.Join(ids, ","), nil
	case "window-terminals":
		for _, window := range f.windows {
			if window.id == args[1] {
				return strings.Join(window.terminals, ","), nil
			}
		}
		return "", nil
	case "terminal-count":
		if f.unhealthy {
			return "0", nil
		}
		want := strings.ToUpper(args[1])
		count := 0
		for _, window := range f.windows {
			for _, id := range window.terminals {
				if strings.ToUpper(id) == want {
					count++
				}
			}
		}
		return strconv.Itoa(count), nil
	case "input-text", "send-key-enter", "focus-terminal":
		return "ok", nil
	case "close-window":
		kept := f.windows[:0]
		for _, window := range f.windows {
			if window.id != args[1] {
				kept = append(kept, window)
			}
		}
		f.windows = kept
		return "ok", nil
	case "close-terminal":
		if len(args) < 2 {
			return "", errors.New("close-terminal requires a terminal id")
		}
		want := strings.ToUpper(args[1])
		keptWindows := f.windows[:0]
		for _, window := range f.windows {
			keptTerms := window.terminals[:0]
			for _, id := range window.terminals {
				if strings.ToUpper(id) != want {
					keptTerms = append(keptTerms, id)
				}
			}
			if len(keptTerms) > 0 {
				window.terminals = keptTerms
				keptWindows = append(keptWindows, window)
			}
		}
		f.windows = keptWindows
		return "ok", nil
	default:
		return "", fmt.Errorf("unknown ghostty op %q", op)
	}
}

func (f *fakeGhostty) nextUUID() string {
	f.seq++
	return fmt.Sprintf("019C5A10-75D8-7EEF-8DB7-%012X", f.seq)
}

func (f *fakeGhostty) windowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.windows)
}

func (f *fakeGhostty) addWindow(id, tabID, term string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windows = append(f.windows, fakeGhosttyWindow{id: id, tabID: tabID, terminals: []string{term}})
}

func (f *fakeGhostty) ops() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.log))
	copy(out, f.log)
	return out
}

func newFakeGhosttyBackend(t *testing.T) (*GhosttyBackend, *fakeGhostty) {
	t.Helper()
	fake := &fakeGhostty{version: "1.3.1"}
	backend := NewGhosttyBackend()
	backend.hostname = func() (string, error) { return "host.test", nil }
	backend.run = fake.run
	backend.healthTimeout = time.Second
	backend.healthPoll = 10 * time.Millisecond
	return backend, fake
}

func ghosttyTestPlan(project, nonce string) Plan {
	return Plan{Version: PlanVersion, Agents: []AgentPlan{
		{Handle: "claude", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7a6"},
		{Handle: "codex", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7a7"},
	}}
}

func writeGhosttySleepAMQ(t *testing.T) string {
	t.Helper()
	fakeAMQ := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(fakeAMQ, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeAMQ
}

func TestGhosttyBackendLifecycleAndRecovery(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := writeGhosttySleepAMQ(t)
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70e7a5"
	plan := ghosttyTestPlan(project, nonce)

	detect := backend.Detect()
	if !detect.Available || detect.Profile.Identity() != "ghostty/darwin/v1" || detect.InstanceIdentity != ghosttyInstancePrefix+ghosttyBundleID {
		t.Fatalf("Detect = %#v", detect)
	}
	if strings.Contains(detect.InstanceIdentity, "1.3") {
		t.Fatalf("instance identity leaked version: %q", detect.InstanceIdentity)
	}

	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if created.Outcome != OutcomeCreated || countGhosttyAgentResources(created.Binding) != 2 {
		t.Fatalf("Create = %#v", created)
	}
	assertNoGhosttyCommandOp(t, fake)

	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectPresent {
		t.Fatalf("Inspect = %#v, %v", inspection, err)
	}

	journal := LaunchJournal{
		Phase: JournalCreated, ProjectIdentity: project, Session: "collab", Plan: plan, LaunchNonce: nonce,
		HostIdentity: detect.HostIdentity, InstanceIdentity: detect.InstanceIdentity,
		Backend: LauncherGhostty, Profile: detect.Profile.Identity(),
		Binding: &created.Binding, Placement: created.Binding.Placement,
	}
	journal.RootIdentity, err = canonicalIdentity(root.Base())
	if err != nil {
		t.Fatal(err)
	}
	journal.RootPhysical, _ = fsq.StableTreeIdentityInfo(root.FileInfo())
	reclaimed, err := backend.Reclaim(ReclaimRequest{Context: context.Background(), Journal: journal, Root: root})
	if err != nil || reclaimed.Status != ReclaimAdoptable || countGhosttyAgentResources(BindingRecord{Resources: ResourceIdentitySet{Resources: reclaimed.Resources}}) != 2 {
		t.Fatalf("Reclaim = %#v, %v", reclaimed, err)
	}
	createdWindow, _, err := parseGhosttyWindowResource(created.Binding)
	if err != nil {
		t.Fatal(err)
	}
	reclaimedWindow, _, err := parseGhosttyWindowResource(reclaimed.Binding)
	if err != nil || reclaimedWindow != createdWindow {
		t.Fatalf("Reclaim window = %q, %v, want %q", reclaimedWindow, err, createdWindow)
	}
	if !reflect.DeepEqual(reclaimed.Binding.Placement, created.Binding.Placement) {
		t.Fatalf("Reclaim placement = %#v, want %#v", reclaimed.Binding.Placement, created.Binding.Placement)
	}
	if !reflect.DeepEqual(reclaimed.Binding.Resources, created.Binding.Resources) {
		t.Fatalf("Reclaim resources = %#v, want %#v", reclaimed.Binding.Resources, created.Binding.Resources)
	}

	intent := journal
	intent.Binding = nil
	unknown, err := backend.Reclaim(ReclaimRequest{Context: context.Background(), Journal: intent, Root: root})
	if err != nil || unknown.Status != ReclaimUnknown {
		t.Fatalf("pre-ID Reclaim = %#v, %v", unknown, err)
	}

	foreignBinding := created.Binding
	foreignBinding.InstanceIdentity = "ghostty-app:foreign"
	foreignInspection, err := backend.Inspect(InspectRequest{Binding: foreignBinding, Root: root})
	if err != nil || foreignInspection.Status != InspectUnknown {
		t.Fatalf("foreign Inspect = %#v, %v", foreignInspection, err)
	}

	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	absent, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || absent.Status != InspectAbsent {
		t.Fatalf("Inspect after Close = %#v, %v", absent, err)
	}
}

func TestGhosttyBackendConformance(t *testing.T) {
	backend, _ := newFakeGhosttyBackend(t)
	RunConformance(t, backend)
}

func TestGhosttyCreateSendsExactLineAfterHealthGate(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := writeGhosttySleepAMQ(t)
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70e8a5")
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.ops()
	if indexOfGhosttyOp(calls, "input-text") < indexOfGhosttyOp(calls, "terminal-count") {
		t.Fatalf("sent text before health: %v", calls)
	}
	wantAMQ, err := filepath.EvalSymlinks(fakeAMQ)
	if err != nil {
		t.Fatal(err)
	}
	want := backend.agentCommand(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: wantAMQ, Root: root}, plan.Agents[0])
	found := false
	for _, argv := range calls {
		if len(argv) >= 3 && argv[0] == "input-text" && argv[2] == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("exact command line not sent: want %q in %v", want, calls)
	}
	_ = created
}

func TestGhosttyHealthTimeoutDoesNotSend(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	fake.unhealthy = true
	backend.healthTimeout = 50 * time.Millisecond
	backend.healthPoll = 10 * time.Millisecond
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70e9a5")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root})
	if err == nil || !strings.Contains(err.Error(), "readiness timed out") {
		t.Fatalf("Create error = %v, want readiness timeout", err)
	}
	if indexOfGhosttyOp(fake.ops(), "input-text") >= 0 {
		t.Fatal("sent text after readiness failure")
	}
	if fake.windowCount() != 0 {
		t.Fatalf("health timeout left orphan windows: %d", fake.windowCount())
	}
}

func TestGhosttyCloseRefusesWindowIDReuseWithOtherTerminals(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eaa5")
	plan.Agents = plan.Agents[:1]
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	if len(fake.windows) != 1 {
		fake.mu.Unlock()
		t.Fatalf("windows = %#v", fake.windows)
	}
	fake.windows[0].terminals = []string{"019C5A10-75D8-7EEF-8DB7-FFFFFFFFFFFF"}
	fake.mu.Unlock()

	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectUnknown || !inspection.ActionRequired {
		t.Fatalf("Inspect = %#v, %v, want unknown", inspection, err)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, want closed because bound terminals are already gone", closed)
	}
	if indexOfGhosttyOp(fake.ops(), "close-window") >= 0 {
		t.Fatalf("close-window invoked for reused window id: %v", fake.ops())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.windows) != 1 || len(fake.windows[0].terminals) != 1 || fake.windows[0].terminals[0] != "019C5A10-75D8-7EEF-8DB7-FFFFFFFFFFFF" {
		t.Fatalf("reused window terminals mutated: %#v", fake.windows)
	}
}

func TestGhosttyListWindowsErrorIsUnknownNotAbsent(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70efa5")
	plan.Agents = plan.Agents[:1]
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	before := len(fake.ops())
	fake.fail = "list-windows"
	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectUnknown || !inspection.ActionRequired {
		t.Fatalf("Inspect = %#v, %v, want unknown action-required", inspection, err)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Outcome != OutcomeActionRequired {
		t.Fatalf("Close = %#v, want action-required", closed)
	}
	if indexOfGhosttyOp(fake.ops()[before:], "close-window") >= 0 || indexOfGhosttyOp(fake.ops()[before:], "close-terminal") >= 0 {
		t.Fatalf("list-windows failure closed a window or terminal: %v", fake.ops()[before:])
	}
	fake.fail = ""
	present, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || present.Status != InspectPresent {
		t.Fatalf("window after list failure = %#v, %v", present, err)
	}
}

func TestGhosttyCrashAfterWindowIsUncertain(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	fake.fail = "split"
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eca5"), AMQPath: writeGhosttySleepAMQ(t), Root: root})
	var definite *DefinitePreCreateError
	if err == nil || errors.As(err, &definite) {
		t.Fatalf("Create error = %v, want uncertain post-create failure", err)
	}
	if fake.windowCount() != 0 {
		t.Fatalf("split failure left orphan windows: %d", fake.windowCount())
	}
}

func TestGhosttyCreateTimeoutDoesNotCloseUnackedWindow(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	backend.createTimeout = 20 * time.Millisecond
	inner := fake.run
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "new-window" {
			if _, err := inner(ctx, args...); err != nil {
				return "", err
			}
			<-ctx.Done()
			return "", fmt.Errorf("osascript new-window: %w", ctx.Err())
		}
		return inner(ctx, args...)
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eaa1")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root})
	if err == nil {
		t.Fatal("Create succeeded after create-call timeout")
	}
	if !strings.Contains(err.Error(), "never guessed") {
		t.Fatalf("Create error = %v, want unacknowledged unknown", err)
	}
	if fake.windowCount() != 1 {
		t.Fatalf("timeout closed unacked window: %d", fake.windowCount())
	}
	if indexOfGhosttyOp(fake.ops(), "close-window") >= 0 {
		t.Fatal("timeout closed an unacknowledged window")
	}
}

func TestGhosttyCreateDoesNotCloseForeignInferredWindow(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	inner := fake.run
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "new-window" {
			fake.addWindow("foreign-window", "tab-foreign", "019C5A10-75D8-7EEF-8DB7-0000000000FF")
			return "", errors.New("injected failure")
		}
		return inner(ctx, args...)
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eaa0")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root})
	if err == nil || !strings.Contains(err.Error(), "never guessed") {
		t.Fatalf("Create error = %v, want unacknowledged unknown", err)
	}
	if fake.windowCount() != 1 {
		t.Fatalf("inferred cleanup destroyed foreign window: %d", fake.windowCount())
	}
	if indexOfGhosttyOp(fake.ops(), "close-window") >= 0 {
		t.Fatal("close-window invoked for an inferred foreign id")
	}
}

func TestGhosttyCreateTimeoutDoesNotCloseAmbiguousWindows(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	backend.createTimeout = 20 * time.Millisecond
	inner := fake.run
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "new-window" {
			if _, err := inner(ctx, args...); err != nil {
				return "", err
			}
			fake.addWindow("tab-group-extra", "tab-extra", "019C5A10-75D8-7EEF-8DB7-0000000000EE")
			<-ctx.Done()
			return "", fmt.Errorf("osascript new-window: %w", ctx.Err())
		}
		return inner(ctx, args...)
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eaa2")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root})
	if err == nil || !strings.Contains(err.Error(), "never guessed") {
		t.Fatalf("Create error = %v, want unacknowledged unknown", err)
	}
	if fake.windowCount() != 2 {
		t.Fatalf("ambiguous timeout closed windows: %d", fake.windowCount())
	}
	if indexOfGhosttyOp(fake.ops(), "close-window") >= 0 {
		t.Fatal("ambiguous timeout guessed a window to close")
	}
}

func TestGhosttyDefaultCreateTimeoutExceedsInspectTimeout(t *testing.T) {
	got := NewGhosttyBackend().createOpTimeout()
	if got <= ghosttyCommandTimeout {
		t.Fatalf("default create timeout %s is not greater than inspect timeout %s", got, ghosttyCommandTimeout)
	}
	if got < 30*time.Second {
		t.Fatalf("default create timeout %s is below 30s", got)
	}
}

func TestGhosttyCreateMissingAMQRefusesBeforeMutation(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eda5")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: filepath.Join(t.TempDir(), "missing-amq"), Root: root})
	var definite *DefinitePreCreateError
	if !errors.As(err, &definite) {
		t.Fatalf("Create error = %v, want definite pre-create refusal", err)
	}
	if indexOfGhosttyOp(fake.ops(), "new-window") >= 0 {
		t.Fatal("missing amq created a ghostty window")
	}
}

func TestGhosttyVersionOutsideEnvelopeIsDegradedNotForeign(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	fake.version = "2.0.0"
	detect := backend.Detect()
	if detect.Available {
		t.Fatal("out-of-range version reported available")
	}
	if detect.InstanceIdentity == "" || len(detect.Degradations) == 0 {
		t.Fatalf("Detect = %#v, want instance identity and degradations", detect)
	}
}

func TestGhosttyUnreachableAppleScriptIsNotForeignContext(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70efb5")
	plan.Agents = plan.Agents[:1]
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	fake.fail = "version"
	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectUnknown || !inspection.ActionRequired {
		t.Fatalf("Inspect = %#v, %v, want unknown action-required", inspection, err)
	}
	if !strings.Contains(inspection.Evidence, "unreachable") {
		t.Fatalf("Inspect evidence = %q, want unreachable", inspection.Evidence)
	}
	if strings.Contains(inspection.Evidence, "different backend context") {
		t.Fatalf("unreachable AppleScript reported as foreign: %q", inspection.Evidence)
	}
}

func TestSupportedGhosttyVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{{"1.3.0", true}, {"1.3.1", true}, {"Ghostty 1.4.0", true}, {"1.2.9", false}, {"2.0.0", false}} {
		if got := supportedGhosttyVersion(tc.version); got != tc.want {
			t.Errorf("supportedGhosttyVersion(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestGhosttyInsidePreferencePrependsOnlyWhenInsideGhosttyNotCmux(t *testing.T) {
	prefs := []string{LauncherTMux, LauncherCommands}
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("CMUX_SURFACE_ID", "")
	if got := prependInsideSurfacePreference(prefs); strings.Join(got, ",") != strings.Join(prefs, ",") {
		t.Fatalf("outside prepend = %v", got)
	}
	t.Setenv("TERM_PROGRAM", "ghostty")
	got := prependInsideSurfacePreference(prefs)
	if len(got) != 3 || got[0] != LauncherGhostty || got[1] != LauncherTMux {
		t.Fatalf("inside ghostty prepend = %v", got)
	}
	t.Setenv("CMUX_SURFACE_ID", "F901D722-6789-4BBB-9818-C4E97F20BEB3")
	got = prependInsideSurfacePreference(prefs)
	if len(got) != 3 || got[0] != LauncherCMux || got[1] != LauncherTMux {
		t.Fatalf("inside cmux prepend = %v", got)
	}
}

func TestReconcileAutoSelectsGhosttyWhenInsideGhostty(t *testing.T) {
	tmux := &reconcileBackend{name: LauncherTMux, inspect: InspectAbsent}
	ghostty := &reconcileBackend{name: LauncherGhostty, inspect: InspectAbsent}
	req := reconcileFixture(t, tmux)
	req.Launcher = LauncherAuto
	req.Preferences = []string{LauncherTMux, LauncherGhostty}
	req.Backends[LauncherGhostty] = ghostty
	t.Setenv("TERM_PROGRAM", "ghostty")
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != LauncherGhostty || ghostty.creates != 1 || tmux.creates != 0 {
		t.Fatalf("result=%#v ghostty creates=%d tmux creates=%d", result, ghostty.creates, tmux.creates)
	}
}

func TestReconcileInsideCmuxDoesNotPrependGhostty(t *testing.T) {
	tmux := &reconcileBackend{name: LauncherTMux, inspect: InspectAbsent}
	ghostty := &reconcileBackend{name: LauncherGhostty, inspect: InspectAbsent}
	req := reconcileFixture(t, tmux)
	req.Launcher = LauncherAuto
	req.Preferences = []string{LauncherTMux, LauncherGhostty}
	req.Backends[LauncherGhostty] = ghostty
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("CMUX_SURFACE_ID", "F901D722-6789-4BBB-9818-C4E97F20BEB3")
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != LauncherTMux || tmux.creates != 1 || ghostty.creates != 0 {
		t.Fatalf("result=%#v tmux creates=%d ghostty creates=%d", result, tmux.creates, ghostty.creates)
	}
}

func TestGhosttyArgvRecorderSeesCreateGrammar(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eea5")
	plan.Agents = plan.Agents[:1]
	if _, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root}); err != nil {
		t.Fatal(err)
	}
	joined := fmt.Sprint(fake.ops())
	for _, needle := range []string{"version", "new-window", "terminal-count", "input-text", "send-key-enter"} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("argv log missing %s: %s", needle, joined)
		}
	}
	if strings.Contains(joined, "command") {
		t.Fatalf("create used configuration.command: %s", joined)
	}
}

func TestGhosttyPlacementRowsPassesDown(t *testing.T) {
	backend, fake := newFakeGhosttyBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eea6")
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutRows},
	})
	if err != nil {
		t.Fatal(err)
	}
	foundDown := false
	for _, argv := range fake.ops() {
		if len(argv) > 0 && argv[0] == "split" {
			if len(argv) < 4 || argv[3] != "down" {
				t.Fatalf("rows split argv = %v, want direction down", argv)
			}
			foundDown = true
		}
	}
	if !foundDown {
		t.Fatalf("rows placement issued no split: %v", fake.ops())
	}
	if created.Binding.Placement.Effective.Layout != PlacementLayoutRows {
		t.Fatalf("binding placement = %#v", created.Binding.Placement)
	}
	if _, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, LauncherPane: "%1"},
	}); err == nil || !strings.Contains(err.Error(), PlacementUnsupportedReason) {
		t.Fatalf("unsupported ghostty tuple error = %v", err)
	}
}

func TestGhosttyPlacementStaggersBetweenSplits(t *testing.T) {
	backend, _ := newFakeGhosttyBackend(t)
	var sleeps []time.Duration
	backend.sleep = func(ctx context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	plan := ghosttyTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eea7")
	started := time.Now()
	if _, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeGhosttySleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns, StaggerMS: 250},
	}); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != 250*time.Millisecond {
		t.Fatalf("ghostty stagger sleeps = %v", sleeps)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("ghostty stagger elapsed %s, want at least 250ms", elapsed)
	}
}

func countGhosttyAgentResources(binding BindingRecord) int {
	count := 0
	for _, resource := range binding.Resources.Resources {
		if resource.Agent != "" {
			count++
		}
	}
	return count
}

func assertNoGhosttyCommandOp(t *testing.T, fake *fakeGhostty) {
	t.Helper()
	for _, argv := range fake.ops() {
		for _, arg := range argv {
			if arg == "command" || strings.Contains(arg, "configuration.command") {
				t.Fatalf("ghostty invoked with command: %v", argv)
			}
		}
	}
}

func indexOfGhosttyOp(calls [][]string, op string) int {
	for i, argv := range calls {
		if len(argv) > 0 && argv[0] == op {
			return i
		}
	}
	return -1
}
