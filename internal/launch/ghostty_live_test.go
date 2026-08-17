package launch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGhosttyLive(t *testing.T) {
	if os.Getenv("AMQ_GHOSTTY_LIVE") != "1" {
		t.Skip("AMQ_GHOSTTY_LIVE=1 required; run from a shell inside Ghostty")
	}
	backend := NewGhosttyBackend()
	detect := backend.Detect()
	if !detect.Available {
		t.Fatal("ghostty AppleScript ping failed; grant Automation permission and run inside Ghostty")
	}
	before, err := ghosttyLiveTerminalCount()
	if err != nil {
		t.Fatal(err)
	}
	var sent []string
	inner := backend.run
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "input-text" {
			sent = append(sent, args[2])
		}
		return inner(ctx, args...)
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := writeGhosttySleepAMQ(t)
	resolvedAMQ, err := filepath.EvalSymlinks(fakeAMQ)
	if err != nil {
		t.Fatal(err)
	}
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70efa5"
	plan := ghosttyTestPlan(project, nonce)
	req := CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root}
	created, err := backend.Create(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	})
	if created.Outcome != OutcomeCreated || countGhosttyAgentResources(created.Binding) != 2 {
		t.Fatalf("Create = %#v", created)
	}
	req.AMQPath = resolvedAMQ
	for i, agent := range plan.Agents {
		want := backend.agentCommand(req, agent)
		if i >= len(sent) || sent[i] != want {
			t.Fatalf("sent[%d] = %q, want exact commandLine %q (all=%q)", i, sent, want, sent)
		}
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectPresent {
		t.Fatalf("Inspect present = %#v, %v", inspection, err)
	}
	focused, err := backend.Focus(FocusRequest{Binding: created.Binding, Root: root})
	if err != nil || focused.Outcome != OutcomeAttached {
		t.Fatalf("Focus = %#v, %v", focused, err)
	}

	liveWindows, err := backend.listWindowIDs(context.Background())
	if err != nil || len(liveWindows) == 0 {
		t.Fatalf("list windows = %v, %v", liveWindows, err)
	}
	createdWindow, _, err := parseGhosttyWindowResource(created.Binding)
	if err != nil {
		t.Fatal(err)
	}
	foreignWindow := ""
	for _, id := range liveWindows {
		if id != createdWindow {
			foreignWindow = id
			break
		}
	}
	if foreignWindow == "" {
		t.Fatal("need a pre-existing Ghostty window to prove Close refuses the wrong id")
	}
	forged := created.Binding
	forged.Resources.Resources = append([]ResourceIdentity(nil), created.Binding.Resources.Resources...)
	for i, resource := range forged.Resources.Resources {
		if strings.HasPrefix(resource.OpaqueID, ghosttyWindowPrefix) {
			forged.Resources.Resources[i].OpaqueID = ghosttyWindowPrefix + foreignWindow + ":" + nonce
		}
	}
	refused, err := backend.Close(CloseRequest{Binding: forged, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if refused.Outcome == OutcomeClosed && refused.Reason != "ghostty window already absent" {
		t.Fatalf("Close mutated an unrelated window: %#v", refused)
	}

	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	absent, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || absent.Status != InspectAbsent {
		t.Fatalf("Inspect absent = %#v, %v", absent, err)
	}
	after, err := ghosttyLiveTerminalCount()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("live terminal count changed: before=%d after=%d", before, after)
	}
	if created.Binding.InstanceIdentity != ghosttyInstancePrefix+ghosttyBundleID {
		t.Fatalf("instance identity = %q", created.Binding.InstanceIdentity)
	}
}

func ghosttyLiveTerminalCount() (int, error) {
	cmd := exec.Command("osascript", "-e", `tell application "Ghostty" to count terminals`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}
