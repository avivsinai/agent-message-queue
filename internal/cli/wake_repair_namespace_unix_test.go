//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type fixedWakeAdmissionWatcher struct {
	errors chan error
}

func (watcher fixedWakeAdmissionWatcher) Errors() <-chan error {
	return watcher.errors
}

func TestValidateCanonicalWakeRepairDirectoriesRejectsNamespaceReplacement(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(t *testing.T, root string)
		want    string
	}{
		{
			name: "agent directory",
			replace: func(t *testing.T, root string) {
				t.Helper()
				agentPath := fsq.AgentBase(root, "codex")
				if err := os.Rename(agentPath, agentPath+".detached"); err != nil {
					t.Fatalf("detach agent directory: %v", err)
				}
				if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
					t.Fatalf("create replacement agent directory: %v", err)
				}
			},
			want: "agent directory no longer matches",
		},
		{
			name: "inbox directory",
			replace: func(t *testing.T, root string) {
				t.Helper()
				inboxPath := fsq.AgentInboxNew(root, "codex")
				if err := os.Rename(inboxPath, inboxPath+".detached"); err != nil {
					t.Fatalf("detach inbox directory: %v", err)
				}
				if err := os.Mkdir(inboxPath, 0o700); err != nil {
					t.Fatalf("create replacement inbox directory: %v", err)
				}
			},
			want: "inbox directory no longer matches",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			agentDir, err := openWakeAgentDir(root, "codex")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = agentDir.Close() }()
			inboxDir, err := openWakeRepairInboxDir(agentDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = inboxDir.Close() }()
			source := wakeRepairHandoffSource{
				schema:             wakeRepairHandoffSchema,
				root:               canonicalWakeRoot(root),
				rootIdentity:       "v1:test:1:2",
				agent:              "codex",
				sourceGeneration:   "source-generation",
				sourceTargetDigest: "sha256:" + strings.Repeat("1", 64),
				sourceFloorDigest:  "sha256:" + strings.Repeat("2", 64),
				bootID:             "boot-id",
			}
			if err := source.bindRetainedDirectories(agentDir, inboxDir); err != nil {
				t.Fatal(err)
			}
			if err := validateCanonicalWakeRepairDirectories(root, "codex", source); err != nil {
				t.Fatalf("initial canonical binding: %v", err)
			}

			test.replace(t, root)

			err = validateCanonicalWakeRepairDirectories(root, "codex", source)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("replacement error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(filepath.Join(fsq.AgentBase(root, "codex"), "inbox", "new")); statErr != nil {
				t.Fatalf("replacement namespace is unusable: %v", statErr)
			}
		})
	}
}

func TestValidateWakeRepairChildAdmissionRejectsPendingWatcherRootLoss(t *testing.T) {
	watcherErrors := make(chan error, 1)
	watcherErrors <- errors.New("retained wake inbox directory was renamed or deleted")
	err := validateWakeRepairChildAdmission(
		fixedWakeAdmissionWatcher{errors: watcherErrors},
		"unused-root",
		"codex",
		wakeRepairHandoffSource{},
	)
	if err == nil ||
		!strings.Contains(err.Error(), "wake watcher failed before admission") ||
		!strings.Contains(err.Error(), "retained wake inbox directory was renamed or deleted") {
		t.Fatalf("pending watcher root loss error = %v", err)
	}
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
			inboxPath := fsq.AgentInboxNew(fixture.root, "codex")
			if err := os.Rename(inboxPath, inboxPath+".detached"); err != nil {
				t.Fatalf("detach prepared child inbox: %v", err)
			}
			if err := os.Mkdir(inboxPath, 0o700); err != nil {
				t.Fatalf("create replacement child inbox: %v", err)
			}
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
				"canonical wake repair agent directory no longer matches retained authority",
			},
		},
		{
			name: "direct inbox loss",
			replace: func(t *testing.T, root string) {
				t.Helper()
				inboxPath := fsq.AgentInboxNew(root, "codex")
				if err := os.Rename(inboxPath, inboxPath+".detached"); err != nil {
					t.Fatalf("detach prepared child inbox: %v", err)
				}
				if err := os.Mkdir(inboxPath, 0o700); err != nil {
					t.Fatalf("create replacement child inbox: %v", err)
				}
			},
			wants: []string{
				"wake watcher failed before admission: retained wake inbox directory was renamed or deleted",
				"canonical wake repair inbox directory no longer matches retained authority",
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
			diagnostics := wakeRepairLifecycleDiagnostics(fixture, child)
			logAgentPath := fsq.AgentBase(fixture.root, "codex")
			if test.name == "ancestor agent directory" {
				logAgentPath += ".detached"
			}
			logData, _ := os.ReadFile(filepath.Join(logAgentPath, ".wake.repair.log"))
			evidence := diagnostics + "\nretained repair log:\n" + string(logData)
			matched := false
			for _, want := range test.wants {
				if strings.Contains(evidence, want) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf(
					"child did not report final namespace validation failure\nresult=%#v err=%v\n%s",
					result,
					err,
					evidence,
				)
			}
			assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
		})
	}
}

func TestRepairWakeParentRevalidatesNamespaceAfterChildAcknowledgement(t *testing.T) {
	fixture := newWakeRepairLifecycleFixture(t)
	var child *wakeRepairChild
	barrierReached := false
	detachedAgentPath := fsq.AgentBase(fixture.root, "codex") + ".detached"
	stubRealRepairStarter(
		t,
		func(started *wakeRepairChild, _ error) {
			child = started
		},
		func(started *wakeRepairChild) {
			forceRepairLifecycleChildInspection(t, fixture, started)
			validate := started.validateAdmission
			started.validateAdmission = func() error {
				// The real admission closure invokes this only after the child
				// has echoed the exact admit tuple and before RELEASE.
				agentPath := fsq.AgentBase(fixture.root, "codex")
				if err := os.Rename(agentPath, detachedAgentPath); err != nil {
					return err
				}
				if err := fsq.EnsureAgentDirs(fixture.root, "codex"); err != nil {
					return err
				}
				barrierReached = true
				if _, suppressed := fixture.lineage.floor.Existing["pending.md"]; suppressed {
					t.Fatal("pending lifecycle message is unexpectedly part of inherited suppression floor")
				}
				if _, err := os.Stat(filepath.Join(detachedAgentPath, "inbox", "new", "pending.md")); err != nil {
					t.Fatalf("injectable retained backlog is missing at barrier: %v", err)
				}
				// Hold the parent after ACK. Without an exact RELEASE barrier,
				// the child returns to the loop and injects this backlog.
				time.Sleep(300 * time.Millisecond)
				assertWakeRepairOutputAbsent(
					t,
					fixture.outputPath,
					"while post-ACK RELEASE is withheld",
				)
				return validate()
			}
		},
	)

	result, err := repairWake(fixture.root, "codex")
	if !barrierReached {
		t.Fatal("repair did not reach post-ACK pre-RELEASE validation barrier")
	}
	if err == nil ||
		!strings.Contains(err.Error(), "final wake repair admission validation") ||
		!strings.Contains(err.Error(), "agent directory no longer matches retained authority") {
		t.Fatalf(
			"post-ack namespace replacement result=%#v err=%v\n%s",
			result,
			err,
			wakeRepairLifecycleDiagnostics(fixture, child),
		)
	}
	if result.Status == "repaired" {
		t.Fatalf("post-ack detached namespace returned repaired: %#v", result)
	}
	assertRepairLifecycleChildReapedWithoutClaim(t, fixture, child)
	assertWakeRepairOutputAbsent(t, fixture.outputPath, "after rejected repair child reap")
	for _, directory := range []struct {
		label string
		path  string
	}{
		{label: "canonical replacement", path: fsq.AgentBase(fixture.root, "codex")},
		{label: "detached retained", path: detachedAgentPath},
	} {
		for _, name := range []string{".wake.lock", wakeRepairFloorFileName} {
			path := filepath.Join(directory.path, name)
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("%s claim residue %s remains: %v", directory.label, path, err)
			}
		}
	}
}

func assertWakeRepairOutputAbsent(t *testing.T, path, context string) {
	t.Helper()
	output, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return
	case err != nil:
		t.Fatalf("read wake repair output %s: %v", context, err)
	case len(output) != 0:
		t.Fatalf("wake repair injected %s: %q", context, output)
	}
}
