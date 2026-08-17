package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmuxLive(t *testing.T) {
	if os.Getenv("AMQ_CMUX_LIVE") != "1" {
		t.Skip("AMQ_CMUX_LIVE=1 required; run from a shell inside a cmux surface")
	}
	backend := NewCmuxBackend("")
	detect := backend.Detect()
	if !detect.Available {
		t.Fatalf("cmux Detect unavailable: host=%q instance=%q degradations=%v", detect.HostIdentity, detect.InstanceIdentity, detect.Degradations)
	}
	var sent []string
	inner := backend.run
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		for i, arg := range args {
			if arg == "send" {
				for j := i + 1; j < len(args); j++ {
					if args[j] == "--" && j+1 < len(args) {
						sent = append(sent, args[j+1])
					}
				}
			}
		}
		return inner(ctx, args...)
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := writeSleepAMQ(t)
	resolvedAMQ, err := filepath.EvalSymlinks(fakeAMQ)
	if err != nil {
		t.Fatal(err)
	}
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70efa5"
	plan := cmuxTestPlan(project, nonce)
	req := CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root}
	created, err := backend.Create(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	})
	if created.Outcome != OutcomeCreated || countCmuxAgentResources(created.Binding) != 2 {
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
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	absent, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || absent.Status != InspectAbsent {
		t.Fatalf("Inspect absent = %#v, %v", absent, err)
	}
	if !strings.HasPrefix(created.Binding.InstanceIdentity, "cmux-socket:") {
		t.Fatalf("instance identity = %q", created.Binding.InstanceIdentity)
	}
}
