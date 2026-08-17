package launch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const cursorLivePrompt = "Reply with exactly AMQ_OK. Do not use tools."

func TestCursorLiveResumeManagedExecutionAndCrashReuse(t *testing.T) {
	if os.Getenv("AMQ_CURSOR_LIVE") != "1" {
		t.Skip("set AMQ_CURSOR_LIVE=1 to create one real Cursor chat and run the resume proof")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	adapter := NewCursorAdapter("cursor-agent")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	capabilities := adapter.Capabilities(ctx)
	if !capabilities.Available || !capabilities.Capture || !capabilities.PreSpawnAcquire || capabilities.ProviderVersion != cursorCaptureVersion {
		t.Fatalf("Cursor live capture capability is unavailable: %#v", capabilities)
	}
	t.Logf("Cursor executable: %q", capabilities.Executable)

	fixture := newCursorLiveExecutionFixture(t, capabilities.Executable)
	t.Logf("create-chat argv: %q %q", fixture.ticket.ProviderExecutable, []string{"create-chat"})
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-cursor-live-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { stopTmuxTestServer(t, backend) })

	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{{
		Handle: "cursor", Argv: fixture.ticket.TargetArgv, Cwd: fixture.project,
		AdapterMode: AdapterModeCapture, ResumePolicy: ResumeFresh, LaunchNonce: fixture.nonce,
		Execution: fixture.ticket.Execution,
	}}}
	created, err := backend.Create(CreateRequest{
		ProjectRoot: fixture.project, Session: "collab", Plan: plan,
		AMQPath: fixture.amq, Root: fixture.root,
	})
	if err != nil {
		t.Fatalf("managed tmux/coop-exec create: %v", err)
	}
	if created.Outcome != OutcomeCreated {
		t.Fatalf("managed tmux/coop-exec outcome = %q", created.Outcome)
	}

	acknowledged := waitForCursorLiveTicket(t, fixture.root, fixture.nonce)
	conversationID := acknowledged.ConversationID
	t.Logf("created Cursor chat ID: %s", conversationID)
	if len(acknowledged.EvidenceRefs) != 1 {
		t.Fatalf("acknowledged ticket evidence refs = %#v", acknowledged.EvidenceRefs)
	}
	record, err := LoadConversation(fixture.root, "cursor")
	if err != nil || record.State != CaptureReady || record.Identity != (ConversationIdentity{Provider: CursorProvider, ID: conversationID}) ||
		len(record.EvidenceRefs) != 1 || record.EvidenceRefs[0] != acknowledged.EvidenceRefs[0] {
		t.Fatalf("managed conversation = %#v, %v", record, err)
	}
	evidence, _, err := ReadEvidence(fixture.root, acknowledged.EvidenceRefs[0])
	if err != nil {
		t.Fatalf("read immutable provider evidence: %v", err)
	}
	payload, err := decodeCursorCreateChatPayload(evidence.Payload)
	if err != nil || payload.ConversationID != conversationID || payload.Handle != "cursor" || payload.LaunchNonce != fixture.nonce {
		t.Fatalf("provider evidence payload = %#v, %v", payload, err)
	}

	processArgv := cursorLivePaneArgv(t, backend, capabilities.Executable, conversationID)
	t.Logf("managed provider process argv: %s", processArgv)
	if !strings.Contains(processArgv, "--resume") || !strings.Contains(processArgv, conversationID) {
		t.Fatalf("managed provider process argv does not carry exact resume identity: %s", processArgv)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: fixture.root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("close managed Cursor tmux process: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		cursorLiveResume(t, capabilities.Executable, fixture.project, conversationID, attempt)
	}

	crashFixture := newExecutionFixture(t)
	crashNonce := "85858585-8585-4585-8585-858585858585"
	_, envelope := seedPendingCursorExecution(t, crashFixture, crashNonce)
	acquirer := &countingCursorAcquirer{id: conversationID}
	crash := errors.New("live receipt crash after evidence persistence")
	_, err = prepareExecution(crashFixture.root, "cursor", crashNonce, envelope, acquirer, func(stage string) error {
		if stage == "evidence_persisted" {
			return crash
		}
		return nil
	})
	if !errors.Is(err, crash) {
		t.Fatalf("crash injection error = %v", err)
	}
	retried, err := prepareExecution(crashFixture.root, "cursor", crashNonce, envelope, acquirer, nil)
	if err != nil || retried.State != ExecutionAcknowledged || retried.ConversationID != conversationID || acquirer.calls != 1 {
		t.Fatalf("crash restart = %#v, %v; create-chat calls = %d", retried, err, acquirer.calls)
	}
	t.Logf("crash restart reused Cursor chat ID %s with one acquisition", conversationID)
}

type cursorLiveFixture struct {
	project, amq, nonce string
	root                *fsq.DeliveryRoot
	ticket              ExecutionTicket
}

func newCursorLiveExecutionFixture(t *testing.T, provider string) cursorLiveFixture {
	t.Helper()
	project := t.TempDir()
	git := exec.Command("git", "init", "--quiet")
	git.Dir = project
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("initialize empty Cursor live project: %v\n%s", err, output)
	}
	session := filepath.Join(project, ".agent-mail", "collab")
	if err := fsq.EnsureAgentDirs(session, "cursor"); err != nil {
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
	provider, err = filepath.EvalSymlinks(provider)
	if err != nil {
		t.Fatalf("resolve Cursor executable: %v", err)
	}
	nonce := "84848484-8484-4484-8484-848484848484"
	options := &PrepareExecutionOptions{WakeMode: "disabled", AuditReason: "Cursor live managed resume proof"}
	targetArgv := []string{provider, "--resume", preSpawnConversationPlaceholder}
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "cursor", LaunchNonce: nonce, Mode: AdapterModeCapture, Provider: CursorProvider,
		ProviderVersion: cursorCaptureVersion, PreSpawnAcquire: true,
		Backend: LauncherTMux, Profile: TmuxProfile().Identity(),
		ProjectRoot: project, SessionRoot: session, Cwd: project,
		ProviderExecutable: provider, AMQExecutable: amq, TargetArgv: targetArgv,
		DynamicArgv: []DynamicArg{{Index: 2, Kind: DynamicArgConversationID}}, Execution: options,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("cursor"); err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(root, lease, ConversationRecord{
		Version: ConversationVersion, Handle: "cursor", State: CapturePending,
		ProviderVersion: cursorCaptureVersion, LaunchNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	return cursorLiveFixture{project: project, amq: amq, nonce: nonce, root: root, ticket: ticket}
}

func waitForCursorLiveTicket(t *testing.T, root *fsq.DeliveryRoot, nonce string) ExecutionTicket {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last ExecutionTicket
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = LoadExecutionTicket(root, "cursor")
		if lastErr == nil && last.LaunchNonce == nonce && last.State == ExecutionAcknowledged {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("wait for managed Cursor acknowledgement: ticket=%#v error=%v", last, lastErr)
	return ExecutionTicket{}
}

func cursorLivePaneArgv(t *testing.T, backend *TmuxBackend, provider, conversationID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	output, err := backend.run(ctx, backend.args("list-panes", "-a", "-F", "#{pane_pid}")...)
	if err != nil {
		t.Fatalf("list managed Cursor pane: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || pid <= 0 {
		t.Fatalf("managed Cursor pane pid = %q", strings.TrimSpace(output))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		command := exec.Command("ps", "-ww", "-axo", "pid=,ppid=,command=")
		argv, psErr := command.Output()
		if psErr == nil {
			if found := findCursorLiveDescendantArgv(argv, pid, filepath.Base(provider), conversationID); found != "" {
				return found
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("managed Cursor process %d never exposed resume identity %s", pid, conversationID)
	return ""
}

func findCursorLiveDescendantArgv(output []byte, panePID int, providerBase, conversationID string) string {
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
		if !strings.Contains(candidate.argv, providerBase) || !strings.Contains(candidate.argv, "--resume") || !strings.Contains(candidate.argv, conversationID) {
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

func cursorLiveResume(t *testing.T, executable, cwd, conversationID string, attempt int) {
	t.Helper()
	argv := []string{"--resume", conversationID, "--print", "--trust", "--output-format", "stream-json", "--mode", "ask", "--model", "auto", cursorLivePrompt}
	t.Logf("resume %d argv: %q %q", attempt, executable, argv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, argv...)
	command.Dir = cwd
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.Output()
	if err != nil {
		t.Fatalf("Cursor resume %d failed: %v\nstderr: %s\nstdout: %s", attempt, err, stderr.String(), stdout)
	}
	if err := validateCursorLiveStream(stdout, conversationID); err != nil {
		t.Fatalf("Cursor resume %d stream: %v\nstdout: %s", attempt, err, stdout)
	}
	t.Logf("resume %d PASS: exit 0, one terminal success, all session_id values equal %s", attempt, conversationID)
}

func validateCursorLiveStream(stdout []byte, conversationID string) error {
	type event struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	events, terminal := 0, 0
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var got event
		if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
			return fmt.Errorf("decode NDJSON event %d: %w", events+1, err)
		}
		events++
		if got.SessionID != conversationID {
			return fmt.Errorf("event %d session_id = %q, want %q", events, got.SessionID, conversationID)
		}
		if got.Type == "tool_call" {
			return fmt.Errorf("event %d used a tool despite the fixed no-tool prompt", events)
		}
		if got.Type == "result" {
			if got.Subtype != "success" || got.IsError {
				return fmt.Errorf("terminal event %d is not successful", events)
			}
			terminal++
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if events == 0 || terminal != 1 {
		return fmt.Errorf("events=%d terminal_success=%d, want nonzero and exactly one", events, terminal)
	}
	return nil
}

func TestValidateCursorLiveStream(t *testing.T) {
	id := "018f1f2a-bc34-71bd-9056-23838e27f859"
	terminal := "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"" + id + "\"}\n"
	valid := []byte("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"" + id + "\"}\n" + terminal)
	if err := validateCursorLiveStream(valid, id); err != nil {
		t.Fatalf("valid stream: %v", err)
	}
	for name, hostile := range map[string][]byte{
		"wrong identity": bytes.ReplaceAll(valid, []byte(id), []byte("028f1f2a-bc34-71bd-9056-23838e27f859")),
		"no terminal":    []byte("{\"type\":\"system\",\"session_id\":\"" + id + "\"}\n"),
		"two terminals":  []byte(string(valid) + terminal),
		"tool call":      []byte("{\"type\":\"tool_call\",\"session_id\":\"" + id + "\"}\n" + string(valid)),
		"invalid json":   []byte("not-json\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCursorLiveStream(hostile, id); err == nil {
				t.Fatal("hostile stream was accepted")
			}
		})
	}
}
