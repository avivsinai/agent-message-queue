package launch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const rexLiveStubMarker = "AMQ_REX_LIVE_STUB"

// rexLiveStub prints one marker line naming the handle after --me and the
// Program Resume token it received, then its arguments, and stays alive.
const rexLiveStub = `#!/bin/sh
me=
prev=
for arg do
  if [ "$prev" = "--me" ]; then me=$arg; fi
  prev=$arg
done
printf '` + rexLiveStubMarker + ` me=%s token=[%s]\n' "$me" "${OSC7577_TOKEN-unset}"
printf 'args: %s\n' "$*"
exec sleep 120
`

// TestRexLiveLifecycle drives a real Rex server through Create, Inspect,
// Reclaim, and Close. It must run inside a pane of a scratch Rex session; it
// opens one tab there and closes it.
func TestRexLiveLifecycle(t *testing.T) {
	if os.Getenv("AMQ_REX_LIVE") != "1" {
		t.Skip("set AMQ_REX_LIVE=1 to run the real Rex lifecycle proof from a pane of a scratch Rex session")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("the Rex launcher requires macOS")
	}
	if strings.TrimSpace(os.Getenv("REX_BLOCK")) == "" {
		t.Skip("REX_BLOCK is unset; run this proof from a pane of a scratch Rex session")
	}
	rexPath, err := RexLocator{}.Resolve()
	if err != nil {
		t.Skipf("rex CLI is unavailable: %v", err)
	}
	req := rexCreateRequest(t)
	req.AMQPath = filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(req.AMQPath, []byte(rexLiveStub), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := NewRexBackend("")

	detect := backend.Detect()
	if !detect.Available {
		t.Fatalf("Detect = %#v", detect)
	}
	created, err := backend.Create(req)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		if result, err := backend.Close(CloseRequest{Binding: created.Binding, Root: req.Root}); err != nil || result.Outcome != OutcomeClosed {
			t.Logf("cleanup close = %#v, %v; close the tab named amq-*-%s by hand", result, err, created.Binding.LaunchNonce)
		}
	})
	if created.Outcome != OutcomeCreated || len(created.Binding.Resources.Resources) != 3 {
		t.Fatalf("Create = %#v", created)
	}
	if _, _, err := parseRexWindowResource(created.Binding); err != nil {
		t.Fatalf("tab resource: %v", err)
	}
	blocks, err := rexBoundBlocks(created.Binding)
	if err != nil || len(blocks) != len(req.Plan.Agents) {
		t.Fatalf("pane resources = %v, %v", blocks, err)
	}

	for i, agent := range req.Plan.Agents {
		want := rexLiveStubMarker + " me=" + agent.Handle + " token=[]"
		screen := rexLiveWaitScreen(t, rexPath, blocks[i], want)
		for _, other := range req.Plan.Agents {
			if other.Handle != agent.Handle && strings.Contains(screen, rexLiveStubMarker+" me="+other.Handle+" ") {
				t.Fatalf("pane %s for %s ran the line of %s:\n%s", blocks[i], agent.Handle, other.Handle, screen)
			}
		}
	}

	inspected, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || inspected.Status != InspectPresent {
		t.Fatalf("Inspect = %#v, %v", inspected, err)
	}

	journal := LaunchJournal{Phase: JournalCreated, ProjectIdentity: req.ProjectRoot, Session: req.Session, Plan: req.Plan,
		LaunchNonce: created.Binding.LaunchNonce, HostIdentity: detect.HostIdentity, InstanceIdentity: detect.InstanceIdentity,
		Backend: LauncherRex, Profile: detect.Profile.Identity(), Binding: &created.Binding, Placement: created.Binding.Placement}
	journal.RootIdentity, err = canonicalIdentity(req.Root.Base())
	if err != nil {
		t.Fatal(err)
	}
	journal.RootPhysical, err = fsq.StableTreeIdentityInfo(req.Root.FileInfo())
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := backend.Reclaim(ReclaimRequest{Context: context.Background(), Journal: journal, Root: req.Root})
	if err != nil || reclaimed.Status != ReclaimAdoptable || !reflect.DeepEqual(reclaimed.Binding, created.Binding) {
		t.Fatalf("Reclaim = %#v, %v; want binding %#v", reclaimed, err, created.Binding)
	}

	closedResult, err := backend.Close(CloseRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || closedResult.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closedResult, err)
	}
	closed = true
	inspected, err = backend.Inspect(InspectRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || inspected.Status != InspectAbsent {
		t.Fatalf("Inspect after close = %#v, %v", inspected, err)
	}
}

// rexLiveWaitScreen polls one pane's screen until it contains want.
func rexLiveWaitScreen(t *testing.T, rexPath, block, want string) string {
	t.Helper()
	id := "block:" + block
	deadline := time.Now().Add(30 * time.Second)
	var screen string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, rexPath, "--autostart=false", "capture", "-s", id, "-b", id, "--trim").CombinedOutput()
		cancel()
		screen = string(out)
		if err == nil && strings.Contains(screen, want) {
			return screen
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never showed %q (last capture error %v):\n%s", block, want, err, screen)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
