package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const fakeCmuxScript = `#!/usr/bin/env python3
import json, os, sys, uuid

log_path = os.environ["AMQ_CMUX_FAKE_LOG"]
state_path = os.environ["AMQ_CMUX_FAKE_STATE"]
with open(log_path, "a", encoding="utf-8") as log:
    log.write(json.dumps(sys.argv[1:]) + "\n")

args = sys.argv[1:]
flags = {}
positional = []
i = 0
while i < len(args):
    arg = args[i]
    if arg == "--":
        positional.extend(args[i + 1 :])
        break
    if arg == "--json":
        i += 1
        continue
    if arg == "--id-format" and i + 1 < len(args):
        i += 2
        continue
    if arg.startswith("--") and i + 1 < len(args) and not args[i + 1].startswith("--"):
        flags[arg[2:].replace("-", "_")] = args[i + 1]
        i += 2
        continue
    positional.append(arg)
    i += 1

command = positional[0] if positional else ""
fail = os.environ.get("AMQ_CMUX_FAKE_FAIL", "")
if fail == command or fail == "*":
    print("injected failure", file=sys.stderr)
    sys.exit(1)

def load():
    if not os.path.exists(state_path):
        return {
            "socket_path": os.environ.get("AMQ_CMUX_FAKE_SOCKET", "/tmp/cmux-fake.sock"),
            "version": os.environ.get("AMQ_CMUX_FAKE_VERSION", "0.64.3"),
            "workspaces": [],
        }
    with open(state_path, encoding="utf-8") as fh:
        return json.load(fh)

def save(state):
    with open(state_path, "w", encoding="utf-8") as fh:
        json.dump(state, fh)

def emit(obj):
    print(json.dumps(obj))

state = load()
healthy = os.environ.get("AMQ_CMUX_FAKE_UNHEALTHY") != "1"
missing = os.environ.get("AMQ_CMUX_FAKE_MISSING", "")

if command == "ping":
    emit({"pong": True})
    sys.exit(0)
if command == "version":
    print(os.environ.get("AMQ_CMUX_FAKE_APP_VERSION", "cmux 0.64.3 (83) [aea6cfcde]"))
    sys.exit(0)
if command == "capabilities":
    protocol = os.environ.get("AMQ_CMUX_FAKE_PROTOCOL", "2")
    if protocol == "string":
        cap_version = "0.64.3"
    else:
        cap_version = int(protocol)
    emit({"socket_path": state["socket_path"], "version": cap_version, "protocol": "cmux-socket",
          "methods": ["workspace.list"]})
    sys.exit(0)
if command == "list-workspaces":
    emit({"workspaces": [{"id": ws["id"], "window_id": ws.get("window_id", ""), "current_directory": ws.get("cwd", ""),
                          "title": ws["title"]} for ws in state["workspaces"]]})
    sys.exit(0)
if command == "new-workspace":
    ws_id = str(uuid.uuid4())
    window_id = str(uuid.uuid4())
    pane_id = str(uuid.uuid4())
    surface_id = str(uuid.uuid4())
    state["workspaces"].append({
        "id": ws_id, "title": flags.get("name", ""), "window_id": window_id,
        "cwd": flags.get("cwd", ""),
        "panes": [{"id": pane_id, "surfaces": [{"id": surface_id}]}],
    })
    save(state)
    created = {"id": ws_id, "window_id": window_id}
    if missing == "id":
        del created["id"]
    emit(created)
    sys.exit(0)
if command == "list-panes":
    ws = next((item for item in state["workspaces"] if item["id"] == flags.get("workspace")), None)
    emit({"panes": [{"id": pane["id"]} for pane in (ws["panes"] if ws else [])]})
    sys.exit(0)
if command == "list-pane-surfaces":
    ws = next((item for item in state["workspaces"] if item["id"] == flags.get("workspace")), None)
    pane = None
    if ws:
        pane = next((item for item in ws["panes"] if item["id"] == flags.get("pane")), None)
    emit({"surfaces": [{"id": surface["id"]} for surface in (pane["surfaces"] if pane else [])]})
    sys.exit(0)
if command == "new-split":
    ws = next((item for item in state["workspaces"] if item["id"] == flags.get("workspace")), None)
    if ws is None:
        print("workspace not found", file=sys.stderr)
        sys.exit(1)
    surface_id = str(uuid.uuid4())
    ws["panes"].append({"id": str(uuid.uuid4()), "surfaces": [{"id": surface_id}]})
    save(state)
    emit({"id": surface_id})
    sys.exit(0)
if command == "surface-health":
    ws = next((item for item in state["workspaces"] if item["id"] == flags.get("workspace")), None)
    surfaces = []
    if ws:
        for pane in ws["panes"]:
            for surface in pane["surfaces"]:
                item = {"id": surface["id"], "in_window": healthy}
                if missing == "in_window":
                    del item["in_window"]
                surfaces.append(item)
    emit({"surfaces": surfaces})
    sys.exit(0)
if command in ("send", "send-key", "select-workspace", "focus-window"):
    emit({"ok": True})
    sys.exit(0)
if command == "close-workspace":
    before = len(state["workspaces"])
    state["workspaces"] = [ws for ws in state["workspaces"] if ws["id"] != flags.get("workspace")]
    save(state)
    emit({"ok": True, "closed": before - len(state["workspaces"])})
    sys.exit(0)
print("unknown command " + command, file=sys.stderr)
sys.exit(2)
`

func TestCmuxBackendLifecycleAndRecovery(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := writeSleepAMQ(t)
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70e7a5"
	plan := cmuxTestPlan(project, nonce)

	detect := backend.Detect()
	if !detect.Available || detect.Profile.Identity() != "cmux/darwin/v1" || !strings.HasPrefix(detect.InstanceIdentity, "cmux-socket:") {
		t.Fatalf("Detect = %#v", detect)
	}
	if strings.Contains(detect.InstanceIdentity, "@") || strings.Contains(detect.InstanceIdentity, detect.Profile.VersionRange) {
		t.Fatalf("instance identity leaked version: %q", detect.InstanceIdentity)
	}

	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if created.Outcome != OutcomeCreated {
		t.Fatalf("Create = %#v", created)
	}
	assertNoCmuxCommandFlag(t, logPath)
	if agents := countCmuxAgentResources(created.Binding); agents != 2 {
		t.Fatalf("binding agents = %d, want 2: %#v", agents, created.Binding.Resources)
	}

	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectPresent {
		t.Fatalf("Inspect = %#v, %v", inspection, err)
	}

	journal := LaunchJournal{
		ProjectIdentity: project, Session: "collab", Plan: plan, LaunchNonce: nonce,
		HostIdentity: detect.HostIdentity, InstanceIdentity: detect.InstanceIdentity,
		Backend: LauncherCMux, Profile: detect.Profile.Identity(),
	}
	journal.RootIdentity, err = canonicalIdentity(root.Base())
	if err != nil {
		t.Fatal(err)
	}
	journal.RootPhysical, _ = fsq.StableTreeIdentityInfo(root.FileInfo())
	reclaimed, err := backend.Reclaim(ReclaimRequest{Context: context.Background(), Journal: journal, Root: root})
	if err != nil || reclaimed.Status != ReclaimAdoptable || countCmuxAgentResources(BindingRecord{Resources: ResourceIdentitySet{Resources: reclaimed.Resources}}) != 2 {
		t.Fatalf("Reclaim = %#v, %v", reclaimed, err)
	}

	if _, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root}); err == nil {
		t.Fatal("duplicate Create succeeded")
	}
	afterDuplicate, _ := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if afterDuplicate.Status != InspectPresent {
		t.Fatalf("duplicate Create changed live resource: %#v", afterDuplicate)
	}

	foreignBinding := created.Binding
	foreignBinding.InstanceIdentity = "cmux-socket:/foreign"
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

func TestCmuxBackendConformance(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
	RunConformance(t, backend)
}

func TestCmuxCreateSendsExactLineAfterHealthGate(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := writeSleepAMQ(t)
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70e8a5")
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	calls := readCmuxArgvLog(t, logPath)
	if indexOfCmuxCommand(calls, "send") < indexOfCmuxCommand(calls, "surface-health") {
		t.Fatalf("sent text before surface-health: %v", calls)
	}
	wantAMQ, err := filepath.EvalSymlinks(fakeAMQ)
	if err != nil {
		t.Fatal(err)
	}
	want := backend.agentCommand(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: wantAMQ, Root: root}, plan.Agents[0])
	found := false
	for _, argv := range calls {
		if len(argv) >= 1 && argv[0] == "send" {
			for i, arg := range argv {
				if arg == "--" && i+1 < len(argv) && argv[i+1] == want {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("exact command line not sent: want %q in %v", want, calls)
	}
	_ = created
}

func TestCmuxHealthTimeoutDoesNotSend(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_UNHEALTHY", "1")
	backend.healthTimeout = 50 * time.Millisecond
	backend.healthPoll = 10 * time.Millisecond
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70e9a5")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err == nil || !strings.Contains(err.Error(), "readiness timed out") {
		t.Fatalf("Create error = %v, want readiness timeout", err)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "send") >= 0 {
		t.Fatal("sent text after readiness failure")
	}
	if cmuxFakeWorkspaceCount(t) != 0 {
		t.Fatalf("health timeout left orphan workspaces: %d", cmuxFakeWorkspaceCount(t))
	}
}

func TestCmuxCloseRefusesDifferentUUIDSameName(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eaa5")
	plan.Agents = plan.Agents[:1]
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	forged := created.Binding
	forged.Resources.Resources = append([]ResourceIdentity(nil), created.Binding.Resources.Resources...)
	for i, resource := range forged.Resources.Resources {
		if strings.HasPrefix(resource.OpaqueID, cmuxWorkspacePrefix) {
			forged.Resources.Resources[i].OpaqueID = cmuxWorkspacePrefix + "019c5a10-75d8-7eef-8db7-5ee77f70eaaa:" + plan.Agents[0].LaunchNonce
		}
	}
	closed, err := backend.Close(CloseRequest{Binding: forged, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Outcome == OutcomeClosed && closed.Reason != "cmux workspace already absent" {
		t.Fatalf("Close mutated an unrelated workspace: %#v", closed)
	}
	for _, argv := range readCmuxArgvLog(t, logPath) {
		if len(argv) > 0 && argv[0] == "close-workspace" {
			t.Fatalf("close-workspace invoked for a missing uuid: %v", argv)
		}
	}
	present, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || present.Status != InspectPresent {
		t.Fatalf("live workspace was closed: %#v, %v", present, err)
	}
}

func TestCmuxMissingIDFailsClosed(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_MISSING", "id")
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eba5")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err == nil || !strings.Contains(err.Error(), "not a UUID") && !strings.Contains(err.Error(), "new-workspace") {
		t.Fatalf("Create error = %v, want fail-closed uuid parse", err)
	}
}

func TestCmuxCrashAfterWorkspaceIsUncertain(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_FAIL", "new-split")
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eca5"), AMQPath: writeSleepAMQ(t), Root: root})
	var definite *DefinitePreCreateError
	if err == nil || errors.As(err, &definite) {
		t.Fatalf("Create error = %v, want uncertain post-create failure", err)
	}
	if !strings.Contains(err.Error(), "closed orphan cmux workspace") {
		t.Fatalf("Create error = %v, want closed orphan", err)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "close-workspace") < 0 {
		t.Fatal("split failure did not close the orphan workspace")
	}
	if cmuxFakeWorkspaceCount(t) != 0 {
		t.Fatalf("split failure left orphan workspaces: %d", cmuxFakeWorkspaceCount(t))
	}
}

func TestCmuxCreateTimeoutClosesSingleOrphanWorkspace(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	backend.createTimeout = 20 * time.Millisecond
	inner := backend.run
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if cmuxArgvHas(args, "new-workspace") {
			if _, err := inner(context.Background(), args...); err != nil {
				return "", err
			}
			<-ctx.Done()
			return "", fmt.Errorf("cmux new-workspace: %w", ctx.Err())
		}
		return inner(ctx, args...)
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eaa1")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err == nil {
		t.Fatal("Create succeeded after create-call timeout")
	}
	if !strings.Contains(err.Error(), "closed orphan cmux workspace") {
		t.Fatalf("Create error = %v, want closed orphan", err)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "close-workspace") < 0 {
		t.Fatal("timeout did not close the orphan workspace")
	}
	if cmuxFakeWorkspaceCount(t) != 0 {
		t.Fatalf("timeout left orphan workspaces: %d", cmuxFakeWorkspaceCount(t))
	}
}

func TestCmuxCreateTimeoutDoesNotCloseAmbiguousWorkspaces(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	backend.createTimeout = 20 * time.Millisecond
	inner := backend.run
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if cmuxArgvHas(args, "new-workspace") {
			if _, err := inner(context.Background(), args...); err != nil {
				return "", err
			}
			duplicateCmuxFakeNamedWorkspace(t)
			<-ctx.Done()
			return "", fmt.Errorf("cmux new-workspace: %w", ctx.Err())
		}
		return inner(ctx, args...)
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eaa2")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err == nil || !strings.Contains(err.Error(), "ambiguous cmux workspaces") {
		t.Fatalf("Create error = %v, want ambiguous unknown", err)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "close-workspace") >= 0 {
		t.Fatal("ambiguous timeout guessed a workspace to close")
	}
	if cmuxFakeWorkspaceCount(t) != 2 {
		t.Fatalf("ambiguous timeout closed workspaces: %d", cmuxFakeWorkspaceCount(t))
	}
}

func TestCmuxDefaultCreateTimeoutExceedsCommandTimeout(t *testing.T) {
	got := NewCmuxBackend("").createOpTimeout()
	if got <= cmuxCommandTimeout {
		t.Fatalf("default create timeout %s is not greater than command timeout %s", got, cmuxCommandTimeout)
	}
	if got < 30*time.Second {
		t.Fatalf("default create timeout %s is below 30s", got)
	}
}

func TestCmuxCreateMissingAMQRefusesBeforeMutation(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eda5")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: filepath.Join(t.TempDir(), "missing-amq"), Root: root})
	var definite *DefinitePreCreateError
	if !errors.As(err, &definite) {
		t.Fatalf("Create error = %v, want definite pre-create refusal", err)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "new-workspace") >= 0 {
		t.Fatal("missing amq created a cmux workspace")
	}
}

func TestCmuxVersionOutsideEnvelopeIsDegradedNotForeign(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_APP_VERSION", "cmux 1.0.0 (1) [deadbeef]")
	detect := backend.Detect()
	if detect.Available {
		t.Fatal("out-of-range version reported available")
	}
	if detect.InstanceIdentity == "" || len(detect.Degradations) == 0 {
		t.Fatalf("Detect = %#v, want instance identity and degradations", detect)
	}
	if !strings.Contains(detect.Degradations[0].Reason, "cmux version") {
		t.Fatalf("degradation = %q, want CLI version refusal", detect.Degradations[0].Reason)
	}
}

func TestCmuxCapabilitiesStringVersionIsUnsupported(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_PROTOCOL", "string")
	detect := backend.Detect()
	if detect.Available {
		t.Fatal("string capabilities.version reported available")
	}
	if detect.InstanceIdentity == "" {
		t.Fatal("string version lost instance identity")
	}
	if len(detect.Degradations) == 0 || !strings.Contains(detect.Degradations[0].Reason, "protocol integer") {
		t.Fatalf("degradations = %#v, want typed protocol refusal", detect.Degradations)
	}
}

func TestCmuxProtocolVersionUnsupported(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_PROTOCOL", "3")
	detect := backend.Detect()
	if detect.Available {
		t.Fatal("protocol 3 reported available")
	}
	if len(detect.Degradations) == 0 || !strings.Contains(detect.Degradations[0].Reason, "protocol version 3") {
		t.Fatalf("degradations = %#v, want protocol 3 refusal", detect.Degradations)
	}
}

func TestSupportedCmuxVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{{"0.64.3", true}, {"0.65.0", true}, {"0.64.2", false}, {"1.0.0", false}, {"cmux 0.64.3", false}} {
		if got := supportedCmuxVersion(tc.version); got != tc.want {
			t.Errorf("supportedCmuxVersion(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestParseCmuxCLIVersion(t *testing.T) {
	ok, err := parseCmuxCLIVersion("cmux 0.64.3 (83) [aea6cfcde]\n")
	if err != nil || ok != "0.64.3" {
		t.Fatalf("parseCmuxCLIVersion live shape = %q, %v", ok, err)
	}
	for _, raw := range []string{
		"0.64.3",
		"cmux 0.64.3 extra",
		"cmux 0.64.3 (83) [aea6cfcde]\nsecond line",
		"cmux 0.64",
		"cmux v0.64.3",
		"Cmux 0.64.3",
	} {
		if _, err := parseCmuxCLIVersion(raw); err == nil {
			t.Errorf("parseCmuxCLIVersion(%q) succeeded, want error", raw)
		}
	}
}

func TestCmuxListWorkspacesErrorIsUnknownNotAbsent(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := writeSleepAMQ(t)
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70efa5"
	plan := cmuxTestPlan(project, nonce)
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(root, created.Binding.LaunchNonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBinding(root, lease, created.Binding); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenTrustStore(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	detect := backend.Detect()
	req := ReconcileRequest{
		ProjectRoot: project, Session: "collab", AMQPath: fakeAMQ, Root: root,
		Config: ProjectConfig{Schema: ProjectConfigSchema, DefaultSession: "collab", Layout: LayoutIntent{Type: LayoutColumns}, Agents: []ProjectAgentConfig{
			{Handle: "claude", Adapter: "claude", Command: []string{"claude"}, ResumePolicy: ResumeEnabled},
		}},
		Launcher: LauncherCMux, Preferences: []string{LauncherCMux},
		Backends:   map[string]Backend{LauncherCMux: backend},
		Adapters:   map[string]HarnessAdapter{"claude": reconcileAdapter{name: "claude", mode: AdapterModeMint, available: true}},
		TrustStore: store, ConfirmTrust: func(Plan, string) (bool, error) { return true, nil },
		HostIdentity: detect.HostIdentity,
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMQ_CMUX_FAKE_FAIL", "list-workspaces")

	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectUnknown || !inspection.ActionRequired {
		t.Fatalf("Inspect = %#v, %v, want unknown action-required", inspection, err)
	}
	if !strings.Contains(inspection.Evidence, "list cmux workspaces") {
		t.Fatalf("Inspect evidence = %q, want list-workspaces failure", inspection.Evidence)
	}

	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Reason != "inspect_unknown" {
		t.Fatalf("Reconcile = %#v, want inspect_unknown", result)
	}

	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Outcome != OutcomeActionRequired {
		t.Fatalf("Close = %#v, want action-required", closed)
	}

	calls := readCmuxArgvLog(t, logPath)
	if indexOfCmuxCommand(calls, "new-workspace") >= 0 || indexOfCmuxCommand(calls, "close-workspace") >= 0 {
		t.Fatalf("list-workspaces failure mutated backend: %v", calls)
	}

	t.Setenv("AMQ_CMUX_FAKE_FAIL", "")
	present, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || present.Status != InspectPresent {
		t.Fatalf("workspace after list failure = %#v, %v", present, err)
	}
}

func TestCmuxUnreachableSocketIsNotForeignContext(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70efb5")
	plan.Agents = plan.Agents[:1]
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMQ_CMUX_FAKE_FAIL", "ping")
	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectUnknown || !inspection.ActionRequired {
		t.Fatalf("Inspect = %#v, %v, want unknown action-required", inspection, err)
	}
	if !strings.Contains(inspection.Evidence, "unreachable") {
		t.Fatalf("Inspect evidence = %q, want unreachable socket", inspection.Evidence)
	}
	if strings.Contains(inspection.Evidence, "different backend context") {
		t.Fatalf("unreachable socket reported as foreign: %q", inspection.Evidence)
	}
}

func TestCmuxInsidePreferencePrependsOnlyWhenInside(t *testing.T) {
	prefs := []string{LauncherTMux, LauncherCommands}
	if got := prependInsideCmuxPreference(prefs); !strings.EqualFold(strings.Join(got, ","), strings.Join(prefs, ",")) {
		t.Fatalf("outside prepend = %v", got)
	}
	t.Setenv("CMUX_SURFACE_ID", "F901D722-6789-4BBB-9818-C4E97F20BEB3")
	got := prependInsideCmuxPreference(prefs)
	if len(got) != 3 || got[0] != LauncherCMux || got[1] != LauncherTMux {
		t.Fatalf("inside prepend = %v", got)
	}
}

func newFakeCmuxBackend(t *testing.T) (*CmuxBackend, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "cmux")
	if err := os.WriteFile(script, []byte(fakeCmuxScript), 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "argv.log")
	statePath := filepath.Join(dir, "state.json")
	socket := filepath.Join(dir, "cmux.sock")
	t.Setenv("AMQ_CMUX_FAKE_LOG", logPath)
	t.Setenv("AMQ_CMUX_FAKE_STATE", statePath)
	t.Setenv("AMQ_CMUX_FAKE_SOCKET", socket)
	t.Setenv("AMQ_CMUX_FAKE_VERSION", "0.64.3")
	t.Setenv("AMQ_CMUX_FAKE_APP_VERSION", "cmux 0.64.3 (83) [aea6cfcde]")
	t.Setenv("AMQ_CMUX_FAKE_PROTOCOL", "2")
	t.Setenv("AMQ_CMUX_FAKE_FAIL", "")
	t.Setenv("AMQ_CMUX_FAKE_UNHEALTHY", "")
	t.Setenv("AMQ_CMUX_FAKE_MISSING", "")
	backend := NewCmuxBackend(script)
	backend.healthTimeout = time.Second
	backend.healthPoll = 10 * time.Millisecond
	return backend, logPath
}

func writeSleepAMQ(t *testing.T) string {
	t.Helper()
	fakeAMQ := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(fakeAMQ, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeAMQ
}

func cmuxTestPlan(project, nonce string) Plan {
	return Plan{Version: PlanVersion, Agents: []AgentPlan{
		{Handle: "claude", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7a6"},
		{Handle: "codex", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7a7"},
	}}
}

func assertNoCmuxCommandFlag(t *testing.T, logPath string) {
	t.Helper()
	for _, argv := range readCmuxArgvLog(t, logPath) {
		for _, arg := range argv {
			if arg == "--command" {
				t.Fatalf("cmux invoked with --command: %v", argv)
			}
		}
	}
}

func readCmuxArgvLog(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, argv)
	}
	return calls
}

func indexOfCmuxCommand(calls [][]string, command string) int {
	for i, argv := range calls {
		for _, arg := range argv {
			if arg == command {
				return i
			}
		}
	}
	return -1
}

func cmuxArgvHas(args []string, command string) bool {
	for _, arg := range args {
		if arg == command {
			return true
		}
	}
	return false
}

func cmuxFakeWorkspaceCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("AMQ_CMUX_FAKE_STATE"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Workspaces []json.RawMessage `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return len(state.Workspaces)
}

func duplicateCmuxFakeNamedWorkspace(t *testing.T) {
	t.Helper()
	path := os.Getenv("AMQ_CMUX_FAKE_STATE")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	workspaces, _ := state["workspaces"].([]any)
	if len(workspaces) == 0 {
		t.Fatal("no cmux workspace to duplicate")
	}
	first, _ := workspaces[0].(map[string]any)
	clone := map[string]any{
		"id":        "019c5a10-75d8-7eef-8db7-5ee77f70ffff",
		"title":     first["title"],
		"window_id": "019c5a10-75d8-7eef-8db7-5ee77f70fffe",
		"panes":     first["panes"],
	}
	state["workspaces"] = append(workspaces, clone)
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func countCmuxAgentResources(binding BindingRecord) int {
	count := 0
	for _, resource := range binding.Resources.Resources {
		if resource.Agent != "" {
			count++
		}
	}
	return count
}

func TestCmuxArgvRecorderSeesCreateGrammar(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eea5")
	plan.Agents = plan.Agents[:1]
	if _, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root}); err != nil {
		t.Fatal(err)
	}
	joined := fmt.Sprint(readCmuxArgvLog(t, logPath))
	for _, needle := range []string{"ping", "capabilities", "version", "new-workspace", "list-pane-surfaces", "surface-health", "send", "send-key"} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("argv log missing %s: %s", needle, joined)
		}
	}
}
