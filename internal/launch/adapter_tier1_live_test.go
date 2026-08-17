package launch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	claudeLiveVersion      = "2.1.233"
	tier1LivePrompt        = "Reply with exactly AMQ_OK. Do not use tools."
	claudeBootstrapPrompt  = "Reply with exactly AMQ_BOOTSTRAP_OK. Do not use tools."
	tier1LivePrepareDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestClaudeLiveManagedMintResumeAndCrashReuse(t *testing.T) {
	if os.Getenv("AMQ_CLAUDE_LIVE") != "1" {
		t.Skip("set AMQ_CLAUDE_LIVE=1 to create and resume one real Claude session")
	}
	requireTier1LiveTmux(t)
	adapter := NewClaudeAdapter("claude")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	capabilities := adapter.Capabilities(ctx)
	if !capabilities.Available || !capabilities.Fresh || !capabilities.Resume || capabilities.ProviderVersion != claudeLiveVersion {
		t.Fatalf("Claude %s live capability is unavailable: %#v", claudeLiveVersion, capabilities)
	}
	trustedProject := claudeTrustedLiveProject(t)
	fixture := newTier1LiveFixture(t, tier1LiveFixtureRequest{
		handle: "claude", provider: ClaudeProvider, providerVersion: capabilities.ProviderVersion,
		executable: capabilities.Executable, nonce: "86868686-8686-4686-8686-868686868686", mode: AdapterModeMint,
		projectRoot: trustedProject, target: claudeTier1LiveTarget,
	})
	backend, created := startTier1LiveManagedProcess(t, fixture)
	ticket := waitForTier1LiveTicket(t, backend, fixture.root, fixture.handle, fixture.nonce)
	if ticket.ConversationID != fixture.nonce {
		t.Fatalf("Claude ticket identity = %q, want launch nonce %q", ticket.ConversationID, fixture.nonce)
	}
	argv := tier1LiveDescendantArgv(t, backend, filepath.Base(capabilities.Executable), "--session-id", fixture.nonce)
	t.Logf("Claude unused-mint managed argv: %s", argv)
	closeTier1LiveProcess(t, backend, created, fixture.root)

	if err := RevertExecution(fixture.root, fixture.handle, fixture.nonce); err != nil {
		t.Fatal(err)
	}
	assertClaudeUnusedMintStaleAction(t, fixture, capabilities.Executable)

	bootstrap := newTier1LiveFixture(t, tier1LiveFixtureRequest{
		handle: "claude", provider: ClaudeProvider, providerVersion: capabilities.ProviderVersion,
		executable: capabilities.Executable, nonce: fixture.nonce, mode: AdapterModeMint,
		projectRoot: trustedProject, target: claudeTier1BootstrapTarget,
	})
	bootstrapBackend, bootstrapCreated := startTier1LiveManagedProcess(t, bootstrap)
	bootstrapTicket := waitForTier1LiveTicket(t, bootstrapBackend, bootstrap.root, bootstrap.handle, bootstrap.nonce)
	bootstrapArgv := tier1LiveDescendantArgv(t, bootstrapBackend, filepath.Base(capabilities.Executable), "--session-id", bootstrap.nonce, claudeBootstrapPrompt)
	t.Logf("Claude bootstrapped managed argv: %s", bootstrapArgv)
	waitForClaudeLiveSessionRecord(t, bootstrap.project, bootstrap.nonce)
	closeTier1LiveProcess(t, bootstrapBackend, bootstrapCreated, bootstrap.root)

	if err := RevertExecution(bootstrap.root, bootstrap.handle, bootstrap.nonce); err != nil {
		t.Fatal(err)
	}
	retried, err := PrepareExecution(bootstrap.root, bootstrap.handle, bootstrap.nonce, bootstrap.envelope)
	if err != nil || retried.State != ExecutionAcknowledged || retried.ConversationID != fixture.nonce {
		t.Fatalf("Claude crash retry = %#v, %v", retried, err)
	}
	if bootstrapTicket.ConversationID != fixture.nonce {
		t.Fatalf("Claude bootstrap ticket identity = %q, want %q", bootstrapTicket.ConversationID, fixture.nonce)
	}
	claudeLiveResume(t, capabilities.Executable, bootstrap.project, fixture.nonce)
}

func TestCodexLiveManagedAcquireResumeAndCrashReuse(t *testing.T) {
	if os.Getenv("AMQ_CODEX_LIVE") != "1" {
		t.Skip("set AMQ_CODEX_LIVE=1 to create and resume one real Codex thread")
	}
	requireTier1LiveTmux(t)
	adapter := NewCodexAdapter("codex")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	capabilities := adapter.Capabilities(ctx)
	if !capabilities.Available || !capabilities.Fresh || !capabilities.Resume || !capabilities.Capture || capabilities.PreSpawnAcquire || capabilities.ProviderVersion != codexCaptureVersion {
		t.Fatalf("Codex %s live capture capability is unavailable: %#v", codexCaptureVersion, capabilities)
	}
	trustedProject := codexTrustedLiveProject(t)
	pending := newTier1LiveFixture(t, tier1LiveFixtureRequest{
		handle: "codex", provider: CodexProvider, providerVersion: capabilities.ProviderVersion,
		executable: capabilities.Executable, nonce: "87878787-8787-4787-8787-878787878786", mode: AdapterModeCapture,
		projectRoot: trustedProject, target: codexTier1LiveTarget,
	})
	pendingBackend, pendingCreated := startTier1LiveManagedProcess(t, pending)
	_ = waitForCodexLiveTicket(t, pendingBackend, pending.root, pending.handle, pending.nonce)
	pendingArgv := tier1LiveDescendantArgv(t, pendingBackend, filepath.Base(capabilities.Executable), "notify=")
	t.Logf("Codex pending managed argv: %s", pendingArgv)
	closeTier1LiveProcess(t, pendingBackend, pendingCreated, pending.root)
	if err := RevertExecution(pending.root, pending.handle, pending.nonce); err != nil {
		t.Fatal(err)
	}
	assertCodexPendingStaleAction(t, pending, capabilities.Executable)

	fixture := newTier1LiveFixture(t, tier1LiveFixtureRequest{
		handle: "codex", provider: CodexProvider, providerVersion: capabilities.ProviderVersion,
		executable: capabilities.Executable, nonce: "87878787-8787-4787-8787-878787878787", mode: AdapterModeCapture,
		projectRoot: trustedProject, target: codexTier1LiveTarget,
	})
	backend, created := startTier1LiveManagedProcess(t, fixture)
	_ = waitForCodexLiveTicket(t, backend, fixture.root, fixture.handle, fixture.nonce)
	_ = tier1LiveDescendantArgv(t, backend, filepath.Base(capabilities.Executable), "notify=")
	waitForCodexLivePrompt(t, backend)
	ctx, sendCancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer sendCancel()
	if _, err := backend.run(ctx, backend.args("send-keys", "-l", tier1LivePrompt)...); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.run(ctx, backend.args("send-keys", "Enter")...); err != nil {
		t.Fatal(err)
	}
	record := waitForCodexLiveConversation(t, backend, fixture.root, fixture.handle)
	evidenceRecord, _, err := ReadEvidence(fixture.root, record.EvidenceRefs[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, err := decodeCodexNotifyPayload(evidenceRecord.Payload)
	if err != nil || payload.ConversationID != record.Identity.ID || payload.LaunchNonce != fixture.nonce || payload.Handle != fixture.handle {
		t.Fatalf("Codex provider evidence = %#v, %v", payload, err)
	}
	argv := tier1LiveDescendantArgv(t, backend, filepath.Base(capabilities.Executable), "notify=")
	t.Logf("Codex managed argv: %s", argv)
	closeTier1LiveProcess(t, backend, created, fixture.root)
	codexLiveResume(t, capabilities.Executable, fixture.project, record.Identity.ID)
}

type tier1LiveFixtureRequest struct {
	handle, provider, providerVersion, executable, nonce, projectRoot string
	mode                                                              AdapterMode
	preSpawn                                                          bool
	target                                                            func(string, string, string, string, string) ([]string, []DynamicArg, error)
}

type tier1LiveFixture struct {
	handle, project, amq, nonce string
	root                        *fsq.DeliveryRoot
	ticket                      ExecutionTicket
	envelope                    ExecutionEnvelope
}

func TestTier1LiveHarnessTicketsValidateWithVersionManagerSymlinks(t *testing.T) {
	_, claude := testExecutable(t, ClaudeProvider)
	claudeFixture := newTier1LiveFixture(t, tier1LiveFixtureRequest{
		handle: "claude", provider: ClaudeProvider, providerVersion: claudeLiveVersion,
		executable: claude, nonce: "89898989-8989-4989-8989-898989898989", mode: AdapterModeMint,
		target: claudeTier1LiveTarget,
	})
	if err := claudeFixture.ticket.Validate(); err != nil {
		t.Fatalf("Claude live harness ticket: %v", err)
	}
	claudePrepared, err := PrepareExecution(claudeFixture.root, claudeFixture.handle, claudeFixture.nonce, claudeFixture.envelope)
	if err != nil || claudePrepared.State != ExecutionAcknowledged {
		t.Fatalf("Claude live harness prepare = %#v, %v", claudePrepared, err)
	}
	if err := RevertExecution(claudeFixture.root, claudeFixture.handle, claudeFixture.nonce); err != nil {
		t.Fatal(err)
	}
	assertClaudeUnusedMintStaleAction(t, claudeFixture, claude)
	bootstrapArgv, bootstrapDynamic, err := claudeTier1BootstrapTarget(claude, claudeFixture.nonce, claudeFixture.project, claudeFixture.amq, "")
	if err != nil || bootstrapArgv[len(bootstrapArgv)-1] != claudeBootstrapPrompt ||
		len(bootstrapDynamic) != 1 || bootstrapArgv[bootstrapDynamic[0].Index] != claudeFixture.nonce {
		t.Fatalf("Claude bootstrap target = %q %#v, %v", bootstrapArgv, bootstrapDynamic, err)
	}

	_, codex := testExecutable(t, CodexProvider)
	codexFixture := newTier1LiveFixture(t, tier1LiveFixtureRequest{
		handle: "codex", provider: CodexProvider, providerVersion: codexCaptureVersion,
		executable: codex, nonce: "90909090-9090-4090-8090-909090909090", mode: AdapterModeCapture,
		target: codexTier1LiveTarget,
	})
	if err := codexFixture.ticket.Validate(); err != nil {
		t.Fatalf("Codex live harness ticket: %v", err)
	}
	codexPrepared, err := PrepareExecution(codexFixture.root, codexFixture.handle, codexFixture.nonce, codexFixture.envelope)
	if err != nil || codexPrepared.State != ExecutionSpawnAttempted {
		t.Fatalf("Codex live harness prepare = %#v, %v", codexPrepared, err)
	}
}

func claudeTier1LiveTarget(executable, nonce, project, _, _ string) ([]string, []DynamicArg, error) {
	plan, err := NewClaudeAdapter(executable).PlanFresh(PlanRequest{
		Handle: "claude", ProjectRoot: project, Cwd: project, LaunchNonce: nonce, ResumePolicy: ResumeFresh,
	})
	if err != nil {
		return nil, nil, err
	}
	argv := append([]string{plan.Argv[0], "--tools=", "--permission-mode", "plan"}, plan.Argv[1:]...)
	return argv, []DynamicArg{{Index: len(argv) - 1, Kind: DynamicArgLaunchNonce}}, nil
}

func claudeTier1BootstrapTarget(executable, nonce, project, amq, session string) ([]string, []DynamicArg, error) {
	argv, dynamic, err := claudeTier1LiveTarget(executable, nonce, project, amq, session)
	if err != nil {
		return nil, nil, err
	}
	return append(argv, claudeBootstrapPrompt), dynamic, nil
}

func codexTier1LiveTarget(executable, nonce, project, amq, session string) ([]string, []DynamicArg, error) {
	plan, err := NewCodexAdapter(executable).PlanFresh(PlanRequest{
		Handle: "codex", ProjectRoot: project, Cwd: project, SessionRoot: session, AMQExecutable: amq,
		LaunchNonce: nonce, ResumePolicy: ResumeFresh,
	})
	if err != nil {
		return nil, nil, err
	}
	argv := append([]string(nil), plan.Argv...)
	argv = append(argv, "--sandbox", "read-only")
	return argv, nil, nil
}

func newTier1LiveFixture(t *testing.T, request tier1LiveFixtureRequest) tier1LiveFixture {
	t.Helper()
	project := request.projectRoot
	if project == "" {
		project = t.TempDir()
		git := exec.Command("git", "init", "--quiet")
		git.Dir = project
		if output, err := git.CombinedOutput(); err != nil {
			t.Fatalf("initialize live project: %v\n%s", err, output)
		}
	}
	session := filepath.Join(t.TempDir(), "collab")
	if err := fsq.EnsureAgentDirs(session, request.handle); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(session)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(session, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	moduleCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	moduleRoot := filepath.Clean(filepath.Join(moduleCwd, "..", ".."))
	amq := filepath.Join(t.TempDir(), "amq")
	build := exec.Command("go", "build", "-o", amq, "./cmd/amq")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build live amq executable: %v\n%s", err, output)
	}
	targetAMQ, err := filepath.EvalSymlinks(amq)
	if err != nil {
		t.Fatal(err)
	}
	targetSession, err := filepath.EvalSymlinks(session)
	if err != nil {
		t.Fatal(err)
	}
	targetArgv, dynamicArgv, err := request.target(request.executable, request.nonce, project, targetAMQ, targetSession)
	if err != nil {
		t.Fatalf("build %s adapter plan: %v", request.provider, err)
	}
	executable, err := filepath.EvalSymlinks(request.executable)
	if err != nil {
		t.Fatalf("resolve %s executable: %v", request.provider, err)
	}
	options := &PrepareExecutionOptions{WakeMode: "disabled", AuditReason: request.provider + " tier-1 live proof"}
	conversationID := ""
	if request.mode == AdapterModeMint {
		conversationID = request.nonce
	}
	backend, profile := "", ""
	if request.preSpawn || (request.provider == CodexProvider && request.mode == AdapterModeCapture) {
		backend, profile = LauncherTMux, TmuxProfile().Identity()
	}
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: request.handle, LaunchNonce: request.nonce, Mode: request.mode, Provider: request.provider,
		ProviderVersion: request.providerVersion, ConversationID: conversationID,
		PreSpawnAcquire: request.preSpawn, Backend: backend, Profile: profile,
		ProjectRoot: project, SessionRoot: session, Cwd: project, ProviderExecutable: executable, AMQExecutable: amq,
		TargetArgv: targetArgv, DynamicArgv: dynamicArgv, Execution: options,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(root, request.nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles(request.handle); err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(root, lease, ConversationRecord{
		Version: ConversationVersion, Handle: request.handle, State: CapturePending,
		ProviderVersion: request.providerVersion, LaunchNonce: request.nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	envelope := ExecutionEnvelope{
		Cwd: project, AMQExecutable: amq, ProviderExecutable: executable,
		TargetArgv: targetArgv, Execution: options,
	}
	return tier1LiveFixture{handle: request.handle, project: project, amq: amq, nonce: request.nonce, root: root, ticket: ticket, envelope: envelope}
}

func startTier1LiveManagedProcess(t *testing.T, fixture tier1LiveFixture) (*TmuxBackend, CreateResult) {
	t.Helper()
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-%s-live-%d-%d", fixture.handle, os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { stopTmuxTestServer(t, backend) })
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	if _, err := backend.run(ctx, backend.args(
		"start-server", ";", "set-option", "-g", "exit-empty", "off", ";", "set-window-option", "-g", "remain-on-exit", "on",
	)...); err != nil {
		t.Fatalf("configure live diagnostic tmux server: %v", err)
	}
	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{{
		Handle: fixture.handle, Argv: fixture.ticket.TargetArgv, DynamicArgv: fixture.ticket.DynamicArgv,
		Cwd: fixture.project, AdapterMode: fixture.ticket.Mode, ResumePolicy: ResumeFresh,
		LaunchNonce: fixture.nonce, ConversationID: fixture.ticket.ConversationID,
		PreSpawnAcquire: fixture.ticket.PreSpawnAcquire, Execution: fixture.ticket.Execution,
	}}}
	created, err := backend.Create(CreateRequest{ProjectRoot: fixture.project, Session: "collab", Plan: plan, AMQPath: fixture.amq, Root: fixture.root})
	if err != nil || created.Outcome != OutcomeCreated {
		t.Fatalf("managed tmux/coop-exec create = %#v, %v", created, err)
	}
	return backend, created
}

func waitForTier1LiveTicket(t *testing.T, backend *TmuxBackend, root *fsq.DeliveryRoot, handle, nonce string) ExecutionTicket {
	return waitForTier1LiveTicketState(t, backend, root, handle, nonce, ExecutionAcknowledged)
}

func waitForCodexLiveTicket(t *testing.T, backend *TmuxBackend, root *fsq.DeliveryRoot, handle, nonce string) ExecutionTicket {
	return waitForTier1LiveTicketState(t, backend, root, handle, nonce, ExecutionSpawnAttempted)
}

func waitForTier1LiveTicketState(t *testing.T, backend *TmuxBackend, root *fsq.DeliveryRoot, handle, nonce string, state ExecutionState) ExecutionTicket {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last ExecutionTicket
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = LoadExecutionTicket(root, handle)
		if lastErr == nil && last.LaunchNonce == nonce && last.State == state {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	diagnostic := ""
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	if output, err := backend.run(ctx, backend.args("capture-pane", "-p", "-S", "-200")...); err == nil {
		diagnostic = strings.TrimSpace(output)
	} else {
		diagnostic = "capture pane: " + err.Error()
	}
	t.Fatalf("wait for managed %s state %s: ticket=%#v error=%v\npane:\n%s", handle, state, last, lastErr, diagnostic)
	return ExecutionTicket{}
}

func waitForCodexLiveConversation(t *testing.T, backend *TmuxBackend, root *fsq.DeliveryRoot, handle string) ConversationRecord {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last ConversationRecord
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = LoadConversation(root, handle)
		if lastErr == nil && last.State == CaptureReady && last.Identity.Provider == CodexProvider &&
			validUUIDv7(last.Identity.ID) && len(last.EvidenceRefs) == 1 {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	diagnostic := ""
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	if output, err := backend.run(ctx, backend.args("capture-pane", "-p", "-S", "-200")...); err == nil {
		diagnostic = strings.TrimSpace(output)
	} else {
		diagnostic = "capture pane: " + err.Error()
	}
	t.Fatalf("wait for Codex notify promotion: record=%#v error=%v\npane:\n%s", last, lastErr, diagnostic)
	return ConversationRecord{}
}

func waitForCodexLivePrompt(t *testing.T, backend *TmuxBackend) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		output, err := backend.run(ctx, backend.args("capture-pane", "-p", "-S", "-80")...)
		cancel()
		if err == nil {
			last = output
			if strings.Contains(output, "OpenAI Codex") && strings.Contains(output, "›") &&
				strings.LastIndex(output, "model:") > strings.LastIndex(output, "model:     loading") {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Codex live prompt did not become ready:\n%s", strings.TrimSpace(last))
}

func tier1LiveDescendantArgv(t *testing.T, backend *TmuxBackend, required ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	output, err := backend.run(ctx, backend.args("list-panes", "-a", "-F", "#{pane_pid}")...)
	if err != nil {
		t.Fatal(err)
	}
	panePID, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || panePID <= 0 {
		t.Fatalf("managed pane pid = %q", strings.TrimSpace(output))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		processes, psErr := exec.Command("ps", "-ww", "-axo", "pid=,ppid=,command=").Output()
		if psErr == nil {
			if found := findTier1LiveDescendantArgv(processes, panePID, required...); found != "" {
				return found
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("managed descendant never exposed required argv %q", required)
	return ""
}

func findTier1LiveDescendantArgv(output []byte, panePID int, required ...string) string {
	type process struct {
		parent int
		argv   string
	}
	processes := make(map[int]process)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr == nil && parentErr == nil {
			processes[pid] = process{parent: parent, argv: strings.Join(fields[2:], " ")}
		}
	}
	for pid, candidate := range processes {
		matches := true
		for _, requiredPart := range required {
			matches = matches && strings.Contains(candidate.argv, requiredPart)
		}
		if !matches {
			continue
		}
		for current := pid; current > 0; current = processes[current].parent {
			if current == panePID {
				return candidate.argv
			}
			if _, ok := processes[current]; !ok {
				break
			}
		}
	}
	return ""
}

func closeTier1LiveProcess(t *testing.T, backend *TmuxBackend, created CreateResult, root *fsq.DeliveryRoot) {
	t.Helper()
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("close managed process = %#v, %v", closed, err)
	}
}

func assertClaudeUnusedMintStaleAction(t *testing.T, fixture tier1LiveFixture, executable string) {
	t.Helper()
	ticket, err := LoadExecutionTicket(fixture.root, fixture.handle)
	if err != nil || ticket.State != ExecutionPending || ticket.ConversationID != fixture.nonce {
		t.Fatalf("unused Claude mint ticket = %#v, %v", ticket, err)
	}
	backend := Commands{}
	detect := backend.Detect()
	result, err := Prepare(context.Background(), PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: fixture.project, SessionRoot: fixture.root.Base(), Session: "collab"},
		Launcher: CommandsBackendName, IntentDigest: tier1LivePrepareDigest,
		Participants: []PrepareParticipant{{
			Handle: fixture.handle, Runnable: true, Provider: ClaudeProvider, Executable: executable,
			Cwd: fixture.project, ResumePolicy: ResumeEnabled, Execution: PrepareExecutionOptions{WakeMode: "disabled"},
		}},
	}, PrepareDependencies{
		Backends: map[string]Backend{CommandsBackendName: backend}, Preferences: []string{CommandsBackendName},
		AdapterFor:   func(_ string, executable string) HarnessAdapter { return NewClaudeAdapter(executable) },
		HostIdentity: detect.HostIdentity,
	})
	if err != nil {
		t.Fatalf("prepare unused Claude mint: %v", err)
	}
	for _, action := range result.RequiredActions {
		if action.Kind == ActionStaleConversation && len(action.Handles) == 1 && action.Handles[0] == fixture.handle &&
			reflect.DeepEqual(action.AllowedDecisions, []string{"fresh_once", "abort"}) {
			t.Logf("Claude unused mint typed action: kind=%s handle=%s conversation_id=%s", action.Kind, fixture.handle, ticket.ConversationID)
			return
		}
	}
	t.Fatalf("unused Claude mint required actions = %#v", result.RequiredActions)
}

func assertCodexPendingStaleAction(t *testing.T, fixture tier1LiveFixture, executable string) {
	t.Helper()
	record, err := LoadConversation(fixture.root, fixture.handle)
	if err != nil || record.State != CapturePending || record.Identity.ID != "" || len(record.EvidenceRefs) != 0 {
		t.Fatalf("pending Codex conversation = %#v, %v", record, err)
	}
	backend := Commands{}
	detect := backend.Detect()
	result, err := Prepare(context.Background(), PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: fixture.project, SessionRoot: fixture.root.Base(), Session: "collab"},
		Launcher: CommandsBackendName, IntentDigest: tier1LivePrepareDigest,
		Participants: []PrepareParticipant{{
			Handle: fixture.handle, Runnable: true, Provider: CodexProvider, Executable: executable,
			Cwd: fixture.project, ResumePolicy: ResumeEnabled, Execution: PrepareExecutionOptions{WakeMode: "disabled"},
		}},
	}, PrepareDependencies{
		Backends: map[string]Backend{CommandsBackendName: backend}, Preferences: []string{CommandsBackendName},
		AdapterFor:   func(_ string, executable string) HarnessAdapter { return NewCodexAdapter(executable) },
		HostIdentity: detect.HostIdentity,
	})
	if err != nil {
		t.Fatalf("prepare pending Codex conversation: %v", err)
	}
	for _, action := range result.RequiredActions {
		if action.Kind == ActionStaleConversation && len(action.Handles) == 1 && action.Handles[0] == fixture.handle &&
			reflect.DeepEqual(action.AllowedDecisions, []string{"fresh_once", "abort"}) {
			t.Logf("Codex pending typed action: kind=%s handle=%s", action.Kind, fixture.handle)
			return
		}
	}
	t.Fatalf("pending Codex required actions = %#v", result.RequiredActions)
}

func waitForClaudeLiveSessionRecord(t *testing.T, project, sessionID string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	projectKey := strings.ReplaceAll(filepath.Clean(project), string(filepath.Separator), "-")
	record := filepath.Join(home, ".claude", "projects", projectKey, sessionID+".jsonl")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		info, statErr := os.Stat(record)
		if statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
			t.Logf("Claude provider session record: %s", record)
			return
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("inspect Claude provider session record: %v", statErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Claude provider session record did not appear for %s", sessionID)
}

func claudeTrustedLiveProject(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "rev-parse", "--git-common-dir")
	command.Dir = cwd
	output, err := command.Output()
	if err != nil {
		t.Fatalf("resolve trusted Claude project: %v", err)
	}
	common := strings.TrimSpace(string(output))
	if !filepath.IsAbs(common) {
		common = filepath.Join(cwd, common)
	}
	common, err = filepath.Abs(common)
	if err != nil || filepath.Base(common) != ".git" {
		t.Fatalf("git common directory = %q, %v", common, err)
	}
	project := filepath.Dir(common)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read Claude trust state: %v", err)
	}
	var config struct {
		Projects map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode Claude trust state: %v", err)
	}
	if _, trusted := config.Projects[project]; !trusted {
		t.Fatalf("Claude live project %q is not already trusted", project)
	}
	t.Logf("Claude trusted live project: %s", project)
	return project
}

func codexTrustedLiveProject(t *testing.T) string {
	t.Helper()
	command := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("resolve Codex trusted checkout: %v", err)
	}
	project := filepath.Dir(strings.TrimSpace(string(output)))
	if info, err := os.Stat(filepath.Join(project, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("Codex trusted checkout is invalid: %s", project)
	}
	return project
}

func claudeLiveResume(t *testing.T, executable, cwd, sessionID string) {
	t.Helper()
	argv := []string{"-p", "--resume", sessionID, "--output-format", "json", "--tools=", "--permission-mode", "plan", tier1LivePrompt}
	t.Logf("Claude headless resume argv: %q %q", executable, argv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, argv...)
	command.Dir = cwd
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		t.Fatalf("Claude headless resume: %v\n%s", err, stderr.String())
	}
	var response struct {
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
	}
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("decode Claude headless result: %v\n%s", err, stdout.String())
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("Claude headless result has trailing JSON: %v", err)
	}
	if response.IsError || response.SessionID != sessionID || strings.TrimSpace(response.Result) != "AMQ_OK" {
		t.Fatalf("Claude headless result = %#v", response)
	}
}

func codexLiveResume(t *testing.T, executable, cwd, threadID string) {
	t.Helper()
	argv := []string{"exec", "--json", "--sandbox", "read-only", "resume", threadID, tier1LivePrompt}
	t.Logf("Codex headless resume argv: %q %q", executable, argv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, argv...)
	command.Dir = cwd
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		t.Fatalf("Codex headless resume: %v\n%s", err, stderr.String())
	}
	var started, completed bool
	var answer string
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode Codex JSONL event: %v\n%s", err, scanner.Bytes())
		}
		switch event.Type {
		case "thread.started":
			if started || event.ThreadID != threadID {
				t.Fatalf("Codex thread.started = %#v, want exact %s", event, threadID)
			}
			started = true
		case "turn.completed":
			completed = true
		case "turn.failed", "error":
			t.Fatalf("Codex resume failure event: %s", scanner.Bytes())
		case "item.started", "item.completed":
			switch event.Item.Type {
			case "agent_message":
				if event.Type == "item.completed" {
					answer = event.Item.Text
				}
			case "reasoning":
			default:
				t.Fatalf("Codex resume emitted a non-message tool item: %s", scanner.Bytes())
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !started || !completed || strings.TrimSpace(answer) != "AMQ_OK" {
		t.Fatalf("Codex headless proof incomplete: started=%t completed=%t answer=%q", started, completed, answer)
	}
}

func requireTier1LiveTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
}
