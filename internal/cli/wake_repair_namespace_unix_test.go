//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type fixedWakeAdmissionWatcher struct {
	errors chan error
}

func (watcher fixedWakeAdmissionWatcher) Errors() <-chan error {
	return watcher.errors
}

func TestRepairWakeRefusesCanonicalInboxReplacementBeforeAdmission(t *testing.T) {
	fixture := newWakeRepairLifecycleFixture(t)
	var child *wakeRepairChild
	var startupDiagnostics string
	stubRealRepairStarter(
		t,
		func(started *wakeRepairChild, startErr error) {
			child = started
			if startErr != nil {
				startupDiagnostics = wakeRepairLifecycleDiagnostics(fixture, started)
			}
		},
		func(started *wakeRepairChild) {
			forceRepairLifecycleChildInspection(t, fixture, started)
			replaceWakeRepairInboxDirectoryAtomicallyForTest(t, fixture.root, "codex")
		},
	)

	result, err := repairWake(fixture.root, "codex")
	if err == nil || !strings.Contains(err.Error(), "inbox directory no longer matches retained authority") {
		t.Fatalf(
			"namespace replacement result=%#v err=%v\n%s",
			result,
			err,
			startupDiagnostics,
		)
	}
	if result.Status == "repaired" {
		t.Fatalf("detached inbox was admitted: %#v", result)
	}
	assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
}

func TestRepairWakeChildAdmissionRejectsNamespaceReplacementAfterParentValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(t *testing.T, root string)
		wants   []string
	}{
		{
			name: "ancestor agent directory",
			replace: func(t *testing.T, root string) {
				t.Helper()
				agentPath := fsq.AgentBase(root, "codex")
				if err := os.Rename(agentPath, agentPath+".detached"); err != nil {
					t.Fatalf("detach prepared child agent directory: %v", err)
				}
				if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
					t.Fatalf("create replacement agent directory: %v", err)
				}
			},
			wants: []string{
				"wake watcher failed before admission: retained wake agent directory was renamed or deleted",
				"canonical wake repair agent directory no longer matches retained authority",
				"retained wake directory namespace validation failed",
				"canonical wake repair agent directory changed while opening",
			},
		},
		{
			name: "direct inbox loss",
			replace: func(t *testing.T, root string) {
				t.Helper()
				replaceWakeRepairInboxDirectoryAtomicallyForTest(t, root, "codex")
			},
			wants: []string{
				"wake watcher failed before admission: retained wake inbox directory was renamed or deleted",
				"canonical wake repair inbox directory no longer matches retained authority",
				// Linux inotify: watcher validates canonical dirs and can lose
				// the SameFile race in openValidatedWakeDirectoryAt (CI run
				// 32046151181 attempt 1). Admission still rejects.
				"retained wake directory namespace validation failed",
				"canonical wake repair inbox directory changed while opening",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWakeRepairLifecycleFixture(t)
			var child *wakeRepairChild
			stubRealRepairStarter(
				t,
				func(started *wakeRepairChild, _ error) {
					child = started
				},
				func(started *wakeRepairChild) {
					forceRepairLifecycleChildInspection(t, fixture, started)
					admit := started.admit
					started.admit = func() error {
						// Admission failure is fail-closed: repairWake reaps
						// this prepared child without a claim. If admission
						// ever retries, a transient namespace observation could
						// leave a child running against detached authority.
						// repairWake has completed its parent-side prepared
						// validation when this closure runs. The child is still
						// blocked waiting for the admit frame.
						test.replace(t, fixture.root)
						return admit()
					}
				},
			)

			result, err := repairWake(fixture.root, "codex")
			if err == nil {
				t.Fatalf("namespace replacement was admitted: %#v", result)
			}
			if result.Status == "repaired" {
				t.Fatalf("detached namespace returned repaired: %#v", result)
			}
			returnedEvidence := result.Reason + "\n" + err.Error()
			matched := false
			for _, want := range test.wants {
				if strings.Contains(returnedEvidence, want) {
					matched = true
					break
				}
			}
			if matched {
				assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
				return
			}

			diagnostics := wakeRepairLifecycleDiagnostics(fixture, child)
			logAgentPath := fsq.AgentBase(fixture.root, "codex")
			if test.name == "ancestor agent directory" {
				logAgentPath += ".detached"
			}
			logData, _ := os.ReadFile(filepath.Join(logAgentPath, ".wake.repair.log"))
			t.Fatalf(
				"child did not return final namespace validation failure\nresult=%#v err=%v\n%s\nretained repair log:\n%s",
				result,
				err,
				diagnostics,
				logData,
			)
		})
	}
}

func replaceWakeRepairInboxDirectoryAtomicallyForTest(t *testing.T, root, me string) {
	t.Helper()
	inboxPath := fsq.AgentInboxNew(root, me)
	replacementPath := inboxPath + ".replacement"
	if err := os.Mkdir(replacementPath, 0o700); err != nil {
		t.Fatalf("create replacement inbox directory: %v", err)
	}
	// A plain os.Rename cannot replace inbox/new once it contains handoff
	// messages. The platform exchange remains atomic and preserves the
	// original directory at replacementPath for post-condition checks.
	if err := exchangeWakeRepairDirectoriesForTest(replacementPath, inboxPath); err != nil {
		t.Fatalf("atomically replace inbox directory: %v", err)
	}
}
