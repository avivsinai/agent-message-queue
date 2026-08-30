//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestOpenWakeRepairInboxDirRejectsSymlinkedInboxComponent(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentPath := fsq.AgentBase(root, "codex")
	inboxPath := filepath.Join(agentPath, "inbox")
	detachedInboxPath := inboxPath + ".detached"
	if err := os.Rename(inboxPath, detachedInboxPath); err != nil {
		t.Fatalf("detach inbox component: %v", err)
	}
	if err := os.Symlink(filepath.Base(detachedInboxPath), inboxPath); err != nil {
		t.Fatalf("replace inbox component with symlink: %v", err)
	}

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if inboxDir != nil {
		_ = inboxDir.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "inbox parent directory") {
		t.Fatalf("symlinked intermediate inbox component error = %v", err)
	}
}

func TestRepairWakeRejectsInboxComponentReplacementBeforeAcknowledgement(t *testing.T) {
	fixture := newWakeRepairLifecycleFixture(t)
	var child *wakeRepairChild
	var detachedInboxPath string
	stubRealRepairStarter(
		t,
		func(started *wakeRepairChild, _ error) {
			child = started
			cleanupRepairLifecycleChild(t, started)
		},
		func(started *wakeRepairChild) {
			forceRepairLifecycleChildInspection(t, fixture, started)
			admit := started.admit
			started.admit = func() error {
				detachedInboxPath = replaceWakeRepairInboxComponentPreservingNew(
					t,
					fixture.root,
					".pre-ack-detached",
				)
				assertWakeRepairOutputAbsent(
					t,
					fixture.outputPath,
					"before acknowledgement after inbox component replacement",
				)
				return admit()
			}
		},
	)

	result, err := repairWake(fixture.root, "codex")
	if detachedInboxPath == "" {
		t.Fatal("repair did not reach the pre-acknowledgement replacement barrier")
	}
	evidence := result.Reason
	if err != nil {
		evidence += "\n" + err.Error()
	}
	if err == nil ||
		(!strings.Contains(
			evidence,
			"inbox parent directory no longer matches retained authority",
		) && !strings.Contains(
			evidence,
			"retained wake inbox directory was renamed or deleted",
		) && !strings.Contains(
			evidence,
			"retained wake inbox parent directory was renamed or deleted",
		) && !strings.Contains(
			evidence,
			"wake directory temporarily unavailable",
		)) {
		t.Fatalf(
			"pre-ack inbox component replacement result=%#v err=%v\n%s",
			result,
			err,
			wakeRepairLifecycleDiagnostics(fixture, child),
		)
	}
	if result.Status == "repaired" {
		t.Fatalf("replaced intermediate inbox component was admitted: %#v", result)
	}
	assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
	assertWakeRepairOutputAbsent(t, fixture.outputPath, "after rejected pre-ack repair")
	assertWakeRepairClaimResidueAbsent(t, []string{
		fsq.AgentBase(fixture.root, "codex"),
		detachedInboxPath,
	})
}

func TestRepairedWakeExitsWithoutInjectingAfterInboxComponentReplacement(t *testing.T) {
	fixture := newWakeRepairLifecycleFixture(t)
	var child *wakeRepairChild
	stubRealRepairStarter(
		t,
		func(started *wakeRepairChild, _ error) {
			child = started
			cleanupRepairLifecycleChild(t, started)
		},
		func(started *wakeRepairChild) {
			stubRepairLifecycleChildInspectionWithoutLockMutation(t, fixture, started)
		},
	)

	result, err := repairWake(fixture.root, "codex")
	if err != nil || result.Status != "repaired" {
		t.Fatalf("repair did not reach RELEASE: result=%#v err=%v", result, err)
	}
	if child == nil || child.Process == nil || child.Waiter == nil {
		t.Fatal("successful repair did not retain its exact child and waiter")
	}

	releasedOutput := waitForWakeRepairOutputLine(t, fixture.outputPath)
	if !bytes.Contains(releasedOutput, []byte(coopWakeDoorbell)) {
		t.Fatalf("released wake output does not contain fixed doorbell: %q", releasedOutput)
	}
	if bytes.Contains(releasedOutput, []byte("must wait for admission")) {
		t.Fatalf("released wake output contains peer-derived message text: %q", releasedOutput)
	}
	releasedOutput = append([]byte(nil), releasedOutput...)

	detachedInboxPath := replaceWakeRepairInboxComponentPreservingNew(
		t,
		fixture.root,
		".post-release-detached",
	)
	writeWakeRepairHandoffMessage(
		t,
		filepath.Join(fsq.AgentInboxNew(fixture.root, "codex"), "late-component.md"),
		"late message after inbox component replacement",
	)

	if err := child.Waiter.waitForExit(5 * time.Second); err != nil {
		t.Fatalf("repaired wake did not exit after inbox component replacement: %v", err)
	}
	if child.Waiter.state == nil {
		t.Fatal("repaired wake exited without a process state")
	}
	if processAlive(child.Process.Pid) {
		t.Fatalf("repaired wake pid %d remains alive after component replacement", child.Process.Pid)
	}

	output, err := os.ReadFile(fixture.outputPath)
	if err != nil {
		t.Fatalf("read repaired wake output after component replacement: %v", err)
	}
	if !bytes.Equal(output, releasedOutput) {
		t.Fatalf(
			"replaced inbox component changed injector output:\nbefore=%q\nafter=%q",
			releasedOutput,
			output,
		)
	}
	assertWakeRepairClaimResidueAbsent(t, []string{
		fsq.AgentBase(fixture.root, "codex"),
		detachedInboxPath,
	})
}

func replaceWakeRepairInboxComponentPreservingNew(
	t *testing.T,
	root, suffix string,
) string {
	t.Helper()
	agentPath := fsq.AgentBase(root, "codex")
	inboxPath := filepath.Join(agentPath, "inbox")
	detachedInboxPath := inboxPath + suffix
	if err := os.Rename(inboxPath, detachedInboxPath); err != nil {
		t.Fatalf("detach inbox component: %v", err)
	}
	if err := os.Mkdir(inboxPath, 0o700); err != nil {
		t.Fatalf("create replacement inbox component: %v", err)
	}
	if err := os.Rename(
		filepath.Join(detachedInboxPath, "new"),
		filepath.Join(inboxPath, "new"),
	); err != nil {
		t.Fatalf("move retained new directory below replacement inbox component: %v", err)
	}
	return detachedInboxPath
}
