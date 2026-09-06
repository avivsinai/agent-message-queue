//go:build !windows

package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/launchapi"
)

type adoptionSmokeFixture struct {
	project     string
	sessionRoot string
	session     string
	amqBinary   string
	claudePath  string
	codexPath   string
	claudeLog   string
	fullstack   string
	env         []string
}

func resolveRealAMQBinary(t *testing.T) string {
	t.Helper()
	if path := strings.TrimSpace(os.Getenv("AMQ_LAUNCH_LIVE_BINARY")); path != "" {
		if !filepath.IsAbs(path) {
			t.Fatalf("AMQ_LAUNCH_LIVE_BINARY must be an absolute path, got %q", path)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("AMQ_LAUNCH_LIVE_BINARY %q: %v", path, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			t.Fatalf("AMQ_LAUNCH_LIVE_BINARY %q is not a file", path)
		}
		return resolved
	}
	return buildAdoptionSmokeAMQ(t)
}

func buildAdoptionSmokeAMQ(t *testing.T) string {
	t.Helper()
	repoRoot, err := cliTestRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	amqBinary := filepath.Join(t.TempDir(), "amq")
	buildTestAMQ(t, repoRoot, amqBinary)
	return amqBinary
}

func rawWakeExecution() *launchapi.ExecutionOptionsV1 {
	return &launchapi.ExecutionOptionsV1{
		RequireWake: true,
		Wake: launchapi.WakeOptionsV1{
			Mode:     launchapi.WakeEnabled,
			Injector: &launchapi.InjectorOptionsV1{Mode: launchapi.InjectorRaw},
		},
	}
}

func (fx adoptionSmokeFixture) legalSquadIntent() launchapi.LaunchIntentV1 {
	return launchapi.LaunchIntentV1{
		IntentVersion: launchapi.IntentVersionV1,
		Participants: []launchapi.ParticipantV1{
			{Handle: "user", Runnable: false},
			{
				Handle: "lead", Runnable: true, Executable: fx.claudePath,
				Args:         []string{"--effort", "high", "--model", "fable"},
				Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: fx.project},
				ResumePolicy: launchapi.ResumePolicyResume, Execution: rawWakeExecution(),
			},
			{
				Handle: "senior-dev", Runnable: true, Executable: fx.codexPath,
				Args:         []string{"--dangerously-bypass-approvals-and-sandbox"},
				Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: fx.project},
				ResumePolicy: launchapi.ResumePolicyResume, Execution: rawWakeExecution(),
			},
			{
				Handle: "fullstack", Runnable: true, Executable: fx.claudePath,
				Args:         []string{"--effort", "high", "--model", "fable"},
				Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: fx.fullstack},
				ResumePolicy: launchapi.ResumePolicyResume, Execution: rawWakeExecution(),
			},
		},
	}
}

func (fx adoptionSmokeFixture) request(intent launchapi.LaunchIntentV1, launcher string) launchapi.PrepareRequestV1 {
	return launchapi.PrepareRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Target:         launchapi.TargetV1{ProjectRoot: fx.project, SessionRoot: fx.sessionRoot, Session: fx.session},
		Launcher:       launcher, Intent: intent,
	}
}

func (fx adoptionSmokeFixture) intentJSON(t *testing.T, intent launchapi.LaunchIntentV1) []byte {
	t.Helper()
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func (fx adoptionSmokeFixture) prepare(t *testing.T, intent launchapi.LaunchIntentV1, launcher string) ([]byte, []byte, int) {
	t.Helper()
	return fx.prepareJSON(t, fx.intentJSON(t, intent), launcher)
}

func (fx adoptionSmokeFixture) prepareJSON(t *testing.T, intentJSON []byte, launcher string) ([]byte, []byte, int) {
	t.Helper()
	intentPath := filepath.Join(t.TempDir(), "intent.json")
	if err := os.WriteFile(intentPath, intentJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", launcher, "--session", fx.session}
	return runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env, args...)
}

func (fx adoptionSmokeFixture) applyEnvelopeJSON(t *testing.T, intent launchapi.LaunchIntentV1, launcher, digest string) []byte {
	t.Helper()
	data, err := json.MarshalIndent(launchapi.ApplyRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Prepare:        fx.request(intent, launcher),
		SubjectDigest:  digest,
		Decisions:      []launchapi.DecisionV1{},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func (fx adoptionSmokeFixture) applyJSONBytes(t *testing.T, applyJSON []byte) ([]byte, []byte, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apply.json")
	if err := os.WriteFile(path, applyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	return runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env, "launch", "--apply", path, "--json")
}

type liveSeatSnapshot struct {
	LaunchTree         []string
	Binding            string
	LeadConversation   string
	SeniorConversation bool
	Journal            bool
	Panes              int
	SessionIDs         int
}

func (fx adoptionSmokeFixture) liveSeatSnapshot(t *testing.T, socketDir string) liveSeatSnapshot {
	t.Helper()
	logData, _ := os.ReadFile(fx.claudeLog)
	binding, err := os.ReadFile(launch.BindingPath(fx.sessionRoot))
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := os.ReadFile(launch.ConversationPath(fx.sessionRoot, "lead"))
	if err != nil {
		t.Fatal(err)
	}
	_, seniorErr := os.Stat(launch.ConversationPath(fx.sessionRoot, "senior-dev"))
	_, journalErr := os.Stat(launch.JournalPath(fx.sessionRoot))
	return liveSeatSnapshot{
		LaunchTree:         snapshotLaunchTree(t, fx.sessionRoot),
		Binding:            string(binding),
		LeadConversation:   string(conversation),
		SeniorConversation: seniorErr == nil,
		Journal:            journalErr == nil,
		Panes:              countHermeticTmuxPanes(t, socketDir),
		SessionIDs:         strings.Count(string(logData), "--session-id "),
	}
}

func snapshotLaunchTree(t *testing.T, sessionRoot string) []string {
	t.Helper()
	root := filepath.Join(sessionRoot, "meta", "launch")
	var names []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		base := filepath.Base(rel)
		if base == "lease.json" || strings.HasSuffix(base, ".lock") {
			return nil
		}
		entry := rel + "\t" + info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += "\t" + snapshotTreeDigestBytes(data)
		}
		names = append(names, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func snapshotTreeDigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func countHermeticTmuxPanes(t *testing.T, socketDir string) int {
	t.Helper()
	cmd := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_pid}")
	cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+socketDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list hermetic tmux panes: %v\n%s", err, output)
	}
	return len(strings.Fields(string(output)))
}
