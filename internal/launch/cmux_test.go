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
    window_id = state.get("window_id", os.environ.get("AMQ_CMUX_FAKE_WINDOW", "019c5a10-75d8-7eef-8db7-5ee77f70aaa0"))
    workspaces = []
    for i, ws in enumerate(state["workspaces"]):
        item = {
            "id": ws["id"], "title": ws["title"], "current_directory": ws.get("cwd", ""),
            "index": i + 1, "selected": bool(ws.get("selected")), "pinned": False,
            "custom_color": None, "description": "", "listening_ports": [],
            "remote": {},
        }
        if missing == "id":
            del item["id"]
        workspaces.append(item)
    emit({"window_id": window_id, "workspaces": workspaces})
    sys.exit(0)
if command == "new-workspace":
    ws_id = str(uuid.uuid4())
    window_id = state.get("window_id") or str(uuid.uuid4())
    pane_id = str(uuid.uuid4())
    surface_id = str(uuid.uuid4())
    ref = str(len(state["workspaces"]) + 1)
    state["window_id"] = window_id
    surfaces = [{"id": surface_id, "type": "terminal"}]
    if os.environ.get("AMQ_CMUX_FAKE_BROWSER") == "1":
        surfaces = [{"id": str(uuid.uuid4()), "type": "browser"}] + surfaces
    state["workspaces"].append({
        "id": ws_id, "title": flags.get("name", ""), "window_id": window_id,
        "cwd": flags.get("cwd", ""), "selected": False, "ref": ref,
        "panes": [{"id": pane_id, "surfaces": surfaces}],
    })
    save(state)
    print("OK workspace:" + ref)
    sys.exit(0)
if command == "list-panes":
    ws = next((item for item in state["workspaces"] if item["id"].lower() == (flags.get("workspace") or "").lower()), None)
    panes = []
    for i, pane in enumerate(ws["panes"] if ws else []):
        surface_ids = [surface["id"] for surface in pane.get("surfaces", [])]
        panes.append({
            "id": pane["id"], "index": i, "focused": False,
            "selected_surface_id": surface_ids[0] if surface_ids else "",
            "surface_ids": surface_ids, "surface_count": len(surface_ids),
            "rows": 24, "columns": 80, "cell_width_px": 8, "cell_height_px": 16,
            "pixel_frame": {},
        })
    emit({"container_frame": {}, "window_id": (ws or {}).get("window_id", ""),
          "workspace_id": (ws or {}).get("id", ""), "panes": panes})
    sys.exit(0)
if command == "list-pane-surfaces":
    ws = next((item for item in state["workspaces"] if item["id"].lower() == (flags.get("workspace") or "").lower()), None)
    pane = None
    if ws:
        pane = next((item for item in ws["panes"] if item["id"] == flags.get("pane")), None)
    emit({"window_id": (ws or {}).get("window_id", ""), "workspace_id": (ws or {}).get("id", ""),
          "pane_id": flags.get("pane", ""),
          "surfaces": [{"id": surface["id"], "type": surface.get("type", "terminal"), "title": "", "index": i, "selected": i == 0}
                       for i, surface in enumerate(pane["surfaces"] if pane else [])]})
    sys.exit(0)
if command == "new-split":
    ws = next((item for item in state["workspaces"] if item["id"].lower() == (flags.get("workspace") or "").lower()), None)
    if ws is None:
        print("workspace not found", file=sys.stderr)
        sys.exit(1)
    surface_id = str(uuid.uuid4())
    pane_id = str(uuid.uuid4())
    ws["panes"].append({"id": pane_id, "surfaces": [{"id": surface_id, "type": "terminal"}]})
    save(state)
    emit({"workspace_id": ws["id"], "surface_id": surface_id, "window_id": ws.get("window_id", ""),
          "pane_id": pane_id, "type": "terminal"})
    sys.exit(0)
if command == "surface-health":
    ws = next((item for item in state["workspaces"] if item["id"].lower() == (flags.get("workspace") or "").lower()), None)
    require_select = os.environ.get("AMQ_CMUX_FAKE_REQUIRE_SELECT") == "1"
    surfaces = []
    idx = 0
    if ws:
        for pane in ws["panes"]:
            for _surface in pane["surfaces"]:
                in_window = healthy
                if require_select:
                    in_window = healthy and bool(ws.get("selected"))
                item = {"index": idx, "ref": "surface:%d" % (idx + 1), "type": _surface.get("type", "terminal"), "in_window": in_window}
                if missing == "in_window":
                    del item["in_window"]
                surfaces.append(item)
                idx += 1
    emit({"window_ref": "window:1", "workspace_ref": "workspace:%s" % (ws or {}).get("ref", "1"),
          "surfaces": surfaces})
    sys.exit(0)
if command == "select-workspace":
    want = (flags.get("workspace") or "").lower()
    for ws in state["workspaces"]:
        ws["selected"] = ws["id"].lower() == want
    save(state)
    print("OK workspace:" + next((ws.get("ref", "1") for ws in state["workspaces"] if ws["id"].lower() == want), "1"))
    sys.exit(0)
if command in ("send", "send-key"):
    if flags.get("surface") and not flags.get("workspace"):
        print("Error: invalid_params: Surface is not a terminal", file=sys.stderr)
        sys.exit(1)
    print("OK surface:11 workspace:7")
    sys.exit(0)
if command == "focus-window":
    emit({"ok": True})
    sys.exit(0)
if command == "close-workspace":
    want = (flags.get("workspace") or "").lower()
    closed = next((ws for ws in state["workspaces"] if ws["id"].lower() == want), None)
    remaining = [ws for ws in state["workspaces"] if ws["id"].lower() != want]
    selected_id = next((ws["id"] for ws in remaining if ws.get("selected")), None)
    if len(remaining) > 1 and selected_id:
        steal = next(ws for ws in remaining if ws["id"] != selected_id)
        for ws in remaining:
            ws["selected"] = ws["id"] == steal["id"]
    state["workspaces"] = remaining
    save(state)
    print("OK workspace:" + (closed or {}).get("ref", "1"))
    sys.exit(0)
if command == "close-surface":
    want = (flags.get("surface") or "").lower()
    remaining = []
    for ws in state["workspaces"]:
        panes = []
        for pane in ws.get("panes", []):
            pane["surfaces"] = [surface for surface in pane.get("surfaces", []) if surface["id"].lower() != want]
            if pane["surfaces"]:
                panes.append(pane)
        ws["panes"] = panes
        if any(pane.get("surfaces") for pane in panes):
            remaining.append(ws)
    state["workspaces"] = remaining
    save(state)
    print("OK surface:" + (flags.get("surface") or "1"))
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
		if len(argv) > 0 && (argv[0] == "close-workspace" || argv[0] == "close-surface" || argv[0] == "close-window") {
			t.Fatalf("%s invoked for a missing uuid: %v", argv[0], argv)
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
	if err == nil || !strings.Contains(err.Error(), "not a UUID") {
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
	afterCreate := false
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if afterCreate {
			assertCmuxOrphanCtxFresh(t, ctx, args)
		}
		if cmuxArgvHas(args, "new-workspace") {
			afterCreate = true
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

func TestCmuxCreateNonJSONClosesOrphanOnFreshContext(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	backend.createTimeout = 20 * time.Millisecond
	inner := backend.run
	afterCreate := false
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if afterCreate {
			assertCmuxOrphanCtxFresh(t, ctx, args)
		}
		if cmuxArgvHas(args, "new-workspace") {
			afterCreate = true
			if _, err := inner(context.Background(), args...); err != nil {
				return "", err
			}
			return "Opened workspace", nil
		}
		return inner(ctx, args...)
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eaa3")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err == nil || !strings.Contains(err.Error(), "parse cmux new-workspace") {
		t.Fatalf("Create error = %v, want parse failure", err)
	}
	if !strings.Contains(err.Error(), "closed orphan cmux workspace") {
		t.Fatalf("Create error = %v, want closed orphan after parse failure", err)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "close-workspace") < 0 {
		t.Fatal("parse failure did not close the orphan workspace")
	}
	if cmuxFakeWorkspaceCount(t) != 0 {
		t.Fatalf("parse failure left orphan workspaces: %d", cmuxFakeWorkspaceCount(t))
	}
}

func TestCmuxCreateAmbiguousTitleAfterAckDoesNotBindOrClose(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	inner := backend.run
	pending := false
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if pending && cmuxArgvHas(args, "list-workspaces") {
			duplicateCmuxFakeNamedWorkspace(t)
			pending = false
		}
		out, err := inner(ctx, args...)
		if cmuxArgvHas(args, "new-workspace") && err == nil {
			pending = true
		}
		return out, err
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eac1")
	plan.Agents = plan.Agents[:1]
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err == nil || created.Outcome == OutcomeCreated {
		t.Fatalf("Create = %#v, %v, want ambiguous refusal", created, err)
	}
	if !strings.Contains(err.Error(), "ambiguous cmux workspaces") {
		t.Fatalf("Create error = %v, want ambiguous title match", err)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "close-workspace") >= 0 {
		t.Fatal("ambiguous title match guessed a workspace to close")
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "send") >= 0 {
		t.Fatal("ambiguous title match sent commands")
	}
	if cmuxFakeWorkspaceCount(t) != 2 {
		t.Fatalf("ambiguous title match closed workspaces: %d", cmuxFakeWorkspaceCount(t))
	}
}

func TestCmuxCreateZeroTitleMatchAfterAckNamesIt(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	inner := backend.run
	pending := false
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if pending && cmuxArgvHas(args, "list-workspaces") {
			hideCmuxFakeWorkspaceTitles(t)
			pending = false
		}
		out, err := inner(ctx, args...)
		if cmuxArgvHas(args, "new-workspace") && err == nil {
			pending = true
		}
		return out, err
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70eac2"
	plan := cmuxTestPlan(project, nonce)
	plan.Agents = plan.Agents[:1]
	name, err := cmuxWorkspaceName(project, "collab", nonce)
	if err != nil {
		t.Fatal(err)
	}
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err == nil || created.Outcome == OutcomeCreated {
		t.Fatalf("Create = %#v, %v, want zero-match refusal", created, err)
	}
	if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "no cmux workspace named") {
		t.Fatalf("Create error = %v, want named zero-match for %q", err, name)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "close-workspace") >= 0 {
		t.Fatal("zero title match guessed a workspace to close")
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "send") >= 0 {
		t.Fatal("zero title match sent commands")
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
	if indexOfCmuxCommand(calls, "new-workspace") >= 0 || indexOfCmuxCommand(calls, "close-workspace") >= 0 || indexOfCmuxCommand(calls, "close-surface") >= 0 {
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
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("CMUX_SURFACE_ID", "")
	if got := prependInsideSurfacePreference(prefs); !strings.EqualFold(strings.Join(got, ","), strings.Join(prefs, ",")) {
		t.Fatalf("outside prepend = %v", got)
	}
	t.Setenv("CMUX_SURFACE_ID", "F901D722-6789-4BBB-9818-C4E97F20BEB3")
	got := prependInsideSurfacePreference(prefs)
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

func TestCmuxPlacementRowsUsesDownSplit(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70e901")
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutRows},
	})
	if err != nil {
		t.Fatal(err)
	}
	foundDown := false
	for _, argv := range readCmuxArgvLog(t, logPath) {
		if cmuxArgvHas(argv, "new-split") && cmuxArgvHas(argv, "down") {
			foundDown = true
		}
		if cmuxArgvHas(argv, "new-split") && cmuxArgvHas(argv, "right") {
			t.Fatalf("rows placement still split right: %v", argv)
		}
	}
	if !foundDown {
		t.Fatalf("rows placement did not split down: %v", readCmuxArgvLog(t, logPath))
	}
	if created.Binding.Placement.Effective.Layout != PlacementLayoutRows {
		t.Fatalf("binding placement = %#v", created.Binding.Placement)
	}
	if _, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns},
	}); err == nil || !strings.Contains(err.Error(), PlacementUnsupportedReason) {
		t.Fatalf("unsupported cmux tuple error = %v", err)
	}
}

func TestCmuxPlacementStaggersBetweenSplits(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
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
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70e902")
	started := time.Now()
	if _, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, StaggerMS: 250},
	}); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != 250*time.Millisecond {
		t.Fatalf("cmux stagger sleeps = %v", sleeps)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("cmux stagger elapsed %s, want at least 250ms", elapsed)
	}
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

func assertCmuxOrphanCtxFresh(t *testing.T, ctx context.Context, args []string) {
	t.Helper()
	if !cmuxArgvHas(args, "list-workspaces") && !cmuxArgvHas(args, "close-workspace") {
		return
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("%v used cancelled context: %v", args, err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("%v missing deadline", args)
	}
	if remain := time.Until(deadline); remain < 20*time.Second {
		t.Fatalf("%v leftover deadline %s, want a fresh create-timeout-scale bound", args, remain)
	}
}

func assertCmuxFocusFalse(t *testing.T, calls [][]string) {
	t.Helper()
	seen := false
	for _, argv := range calls {
		if !cmuxArgvHas(argv, "new-workspace") && !cmuxArgvHas(argv, "new-split") {
			continue
		}
		seen = true
		focus := ""
		for i, arg := range argv {
			if arg == "--focus" && i+1 < len(argv) {
				focus = argv[i+1]
			}
		}
		if focus != "false" {
			t.Fatalf("Create used --focus %q: %v", focus, argv)
		}
	}
	if !seen {
		t.Fatal("Create did not invoke new-workspace")
	}
}

func assertCmuxSendHasWorkspace(t *testing.T, calls [][]string) {
	t.Helper()
	seen := false
	for _, argv := range calls {
		if len(argv) == 0 || (argv[0] != "send" && argv[0] != "send-key") {
			continue
		}
		seen = true
		if !cmuxArgvHas(argv, "--workspace") || !cmuxArgvHas(argv, "--surface") {
			t.Fatalf("%s missing --workspace/--surface: %v", argv[0], argv)
		}
	}
	if !seen {
		t.Fatal("Create did not send")
	}
}

func seedCmuxSelectedWorkspace(t *testing.T) string {
	t.Helper()
	id := "019c5a10-75d8-7eef-8db7-5ee77f70aaa1"
	state := map[string]any{
		"socket_path": os.Getenv("AMQ_CMUX_FAKE_SOCKET"),
		"window_id":   "019c5a10-75d8-7eef-8db7-5ee77f70aaa0",
		"workspaces": []any{
			map[string]any{
				"id": id, "title": "operator-tab", "window_id": "019c5a10-75d8-7eef-8db7-5ee77f70aaa0",
				"cwd": "", "selected": true, "ref": "1",
				"panes": []any{map[string]any{
					"id":       "019c5a10-75d8-7eef-8db7-5ee77f70aaa2",
					"surfaces": []any{map[string]any{"id": "019c5a10-75d8-7eef-8db7-5ee77f70aaa3"}},
				}},
			},
		},
	}
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("AMQ_CMUX_FAKE_STATE"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedCmuxNeighborWorkspace(t *testing.T) {
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
	workspaces = append(workspaces, map[string]any{
		"id": "019c5a10-75d8-7eef-8db7-5ee77f70aaa4", "title": "neighbor-tab",
		"window_id": "019c5a10-75d8-7eef-8db7-5ee77f70aaa0",
		"cwd":       "", "selected": false, "ref": "neighbor",
		"panes": []any{map[string]any{
			"id":       "019c5a10-75d8-7eef-8db7-5ee77f70aaa5",
			"surfaces": []any{map[string]any{"id": "019c5a10-75d8-7eef-8db7-5ee77f70aaa6"}},
		}},
	})
	state["workspaces"] = workspaces
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cmuxFakeSelectedWorkspace(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("AMQ_CMUX_FAKE_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Workspaces []struct {
			ID       string `json:"id"`
			Selected bool   `json:"selected"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range state.Workspaces {
		if workspace.Selected {
			return strings.ToLower(workspace.ID)
		}
	}
	return ""
}

func cmuxFakeTerminalSurfaceID(t *testing.T) string {
	return cmuxFakeSurfaceIDByType(t, "terminal")
}

func cmuxFakeBrowserSurfaceID(t *testing.T) string {
	return cmuxFakeSurfaceIDByType(t, "browser")
}

func cmuxFakeSurfaceIDByType(t *testing.T, wantType string) string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("AMQ_CMUX_FAKE_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Workspaces []struct {
			Panes []struct {
				Surfaces []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"surfaces"`
			} `json:"panes"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range state.Workspaces {
		for _, pane := range workspace.Panes {
			for _, surface := range pane.Surfaces {
				if surface.Type == wantType {
					return strings.ToLower(surface.ID)
				}
			}
		}
	}
	return ""
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
		"selected":  first["selected"],
		"ref":       "dup",
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

func hideCmuxFakeWorkspaceTitles(t *testing.T) {
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
		t.Fatal("no cmux workspace to hide")
	}
	for i, item := range workspaces {
		workspace, _ := item.(map[string]any)
		workspace["title"] = "foreign-title"
		workspaces[i] = workspace
	}
	state["workspaces"] = workspaces
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
	calls := readCmuxArgvLog(t, logPath)
	joined := fmt.Sprint(calls)
	for _, needle := range []string{"ping", "capabilities", "version", "new-workspace", "list-workspaces", "list-panes", "list-pane-surfaces", "surface-health", "send", "send-key"} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("argv log missing %s: %s", needle, joined)
		}
	}
	assertCmuxFocusFalse(t, calls)
	assertCmuxSendHasWorkspace(t, calls)
}

func TestCmuxHealthyCreateDoesNotSelect(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	previous := seedCmuxSelectedWorkspace(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eab1")
	plan.Agents = plan.Agents[:1]
	if _, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root}); err != nil {
		t.Fatal(err)
	}
	if indexOfCmuxCommand(readCmuxArgvLog(t, logPath), "select-workspace") >= 0 {
		t.Fatal("healthy Create stole operator selection")
	}
	if got := cmuxFakeSelectedWorkspace(t); got != previous {
		t.Fatalf("selected workspace = %q, want previous %q", got, previous)
	}
}

func TestCmuxSelectsThenRestoresWhenInWindowFalse(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_REQUIRE_SELECT", "1")
	previous := seedCmuxSelectedWorkspace(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eab2")
	plan.Agents = plan.Agents[:1]
	if _, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root}); err != nil {
		t.Fatal(err)
	}
	var selected []string
	for _, argv := range readCmuxArgvLog(t, logPath) {
		if len(argv) > 0 && argv[0] == "select-workspace" {
			for i, arg := range argv {
				if arg == "--workspace" && i+1 < len(argv) {
					selected = append(selected, strings.ToLower(argv[i+1]))
				}
			}
		}
	}
	if len(selected) < 2 || selected[len(selected)-1] != previous {
		t.Fatalf("select-workspace sequence = %v, want restore to %s", selected, previous)
	}
	if got := cmuxFakeSelectedWorkspace(t); got != previous {
		t.Fatalf("selected workspace = %q, want restored %q", got, previous)
	}
}

func TestCmuxSendFailureRestoresSelection(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_REQUIRE_SELECT", "1")
	t.Setenv("AMQ_CMUX_FAKE_FAIL", "send")
	previous := seedCmuxSelectedWorkspace(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eab3")
	plan.Agents = plan.Agents[:1]
	_, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err == nil || !strings.Contains(err.Error(), "send cmux command") {
		t.Fatalf("Create error = %v, want send failure", err)
	}
	if got := cmuxFakeSelectedWorkspace(t); got != previous {
		t.Fatalf("selected workspace = %q, want restored %q after send failure", got, previous)
	}
}

func TestCmuxCloseRestoresPriorSelection(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	previous := seedCmuxSelectedWorkspace(t)
	seedCmuxNeighborWorkspace(t)
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eab4")
	plan.Agents = plan.Agents[:1]
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if got := cmuxFakeSelectedWorkspace(t); got != previous {
		t.Fatalf("selected after Create = %q, want %q", got, previous)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	if got := cmuxFakeSelectedWorkspace(t); got != previous {
		t.Fatalf("selected workspace = %q, want prior selection %q after Close", got, previous)
	}
	calls := readCmuxArgvLog(t, logPath)
	if indexOfCmuxCommand(calls, "close-workspace") >= 0 || indexOfCmuxCommand(calls, "close-window") >= 0 {
		t.Fatalf("Close issued a workspace/window-wide close: %v", calls)
	}
	if indexOfCmuxCommand(calls, "close-surface") < 0 {
		t.Fatalf("Close did not close owned surfaces: %v", calls)
	}
}

func TestCmuxSendWithoutWorkspaceRejected(t *testing.T) {
	backend, _ := newFakeCmuxBackend(t)
	for _, args := range [][]string{
		{"send", "--surface", "07eee802-4dde-4788-9281-95dd9a4ce502", "--", "hi"},
		{"send-key", "--surface", "07eee802-4dde-4788-9281-95dd9a4ce502", "enter"},
	} {
		_, err := backend.run(context.Background(), args...)
		if err == nil || !strings.Contains(err.Error(), "Surface is not a terminal") {
			t.Fatalf("%s without --workspace error = %v, want invalid_params", args[0], err)
		}
	}
}

func TestCmuxCreateSendsOnlyTerminalSurface(t *testing.T) {
	backend, logPath := newFakeCmuxBackend(t)
	t.Setenv("AMQ_CMUX_FAKE_BROWSER", "1")
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	plan := cmuxTestPlan(project, "019c5a10-75d8-7eef-8db7-5ee77f70eab5")
	plan.Agents = plan.Agents[:1]
	if _, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeSleepAMQ(t), Root: root}); err != nil {
		t.Fatal(err)
	}
	terminal := cmuxFakeTerminalSurfaceID(t)
	browser := cmuxFakeBrowserSurfaceID(t)
	if terminal == "" || browser == "" {
		t.Fatal("fake did not record browser and terminal surfaces")
	}
	for _, argv := range readCmuxArgvLog(t, logPath) {
		if len(argv) == 0 || argv[0] != "send" {
			continue
		}
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, browser) {
			t.Fatalf("send targeted browser surface: %v", argv)
		}
		if !strings.Contains(joined, terminal) {
			t.Fatalf("send missed terminal surface %s: %v", terminal, argv)
		}
	}
}

func TestParseCmuxTerminalSurfaceIDs(t *testing.T) {
	ids, err := parseCmuxTerminalSurfaceIDs(`{"surfaces":[{"id":"019c5a10-75d8-7eef-8db7-5ee77f70aaa1","type":"browser"},{"id":"019c5a10-75d8-7eef-8db7-5ee77f70aaa2","type":"terminal"}]}`)
	if err != nil || len(ids) != 1 || ids[0] != "019c5a10-75d8-7eef-8db7-5ee77f70aaa2" {
		t.Fatalf("parseCmuxTerminalSurfaceIDs = %v, %v", ids, err)
	}
}

func TestParseCmuxOKWorkspaceAck(t *testing.T) {
	if err := parseCmuxOKWorkspaceAck("OK workspace:5\n"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"{\"id\":\"019c5a10-75d8-7eef-8db7-5ee77f70e7a5\"}", "Opened workspace", "OK workspace:", "OK workspace:5\nextra"} {
		if err := parseCmuxOKWorkspaceAck(raw); err == nil {
			t.Errorf("parseCmuxOKWorkspaceAck(%q) succeeded, want error", raw)
		}
	}
}
