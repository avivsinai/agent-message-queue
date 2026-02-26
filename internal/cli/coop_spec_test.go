package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// setupSpecRoot creates a temp root with agent mailboxes and config.json.
func setupSpecRoot(t *testing.T, agents []string) string {
	t.Helper()
	t.Setenv("AM_ROOT", "") // Clear to avoid guardRootOverride conflict with --root
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	cfg := config.Config{
		Version:    format.CurrentVersion,
		CreatedUTC: "2026-01-01T00:00:00Z",
		Agents:     agents,
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	if err := config.WriteConfig(cfgPath, cfg, true); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	return root
}

func TestCoopSpecStart(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	output, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "auth-redesign",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "We need to redesign the auth system.",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("runCoopSpec start: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	if result["topic"] != "auth-redesign" {
		t.Errorf("topic = %v, want auth-redesign", result["topic"])
	}
	if result["phase"] != "research" {
		t.Errorf("phase = %v, want research", result["phase"])
	}
	if result["thread"] != "spec/auth-redesign" {
		t.Errorf("thread = %v, want spec/auth-redesign", result["thread"])
	}

	// Verify state file created
	state, err := loadSpecState(root, "auth-redesign")
	if err != nil {
		t.Fatalf("loadSpecState: %v", err)
	}
	if state.Phase != specPhaseResearch {
		t.Errorf("state.Phase = %s, want research", state.Phase)
	}
	if state.StartedBy != "claude" {
		t.Errorf("state.StartedBy = %s, want claude", state.StartedBy)
	}

	// Verify message delivered to codex inbox
	codexInbox := fsq.AgentInboxNew(root, "codex")
	entries, _ := os.ReadDir(codexInbox)
	if len(entries) != 1 {
		t.Fatalf("expected 1 message in codex inbox, got %d", len(entries))
	}

	// Verify message content
	msgPath := filepath.Join(codexInbox, entries[0].Name())
	msg, err := format.ReadMessageFile(msgPath)
	if err != nil {
		t.Fatalf("ReadMessageFile: %v", err)
	}
	if msg.Header.Kind != format.KindSpecResearch {
		t.Errorf("kind = %s, want spec_research", msg.Header.Kind)
	}
	if msg.Header.Thread != "spec/auth-redesign" {
		t.Errorf("thread = %s, want spec/auth-redesign", msg.Header.Thread)
	}

	// Verify spec dir created
	specDir := fsq.SpecTopicDir(root, "auth-redesign")
	if _, err := os.Stat(specDir); os.IsNotExist(err) {
		t.Errorf("spec dir not created: %s", specDir)
	}
}

func TestCoopSpecStart_DuplicateTopic(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	// Start first
	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "dup-topic",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "First",
		})
	})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Start same topic again
	_, err = captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "dup-topic",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Second",
		})
	})
	if err == nil {
		t.Fatal("expected error for duplicate topic")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCoopSpecStart_InvalidTopic(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "Bad/Topic",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Test",
		})
	})
	if err == nil {
		t.Fatal("expected error for invalid topic")
	}
}

func TestCoopSpecStart_SamePartner(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "test",
			"--partner", "claude",
			"--me", "claude",
			"--root", root,
			"--body", "Test",
		})
	})
	if err == nil {
		t.Fatal("expected error for same partner")
	}
	if !strings.Contains(err.Error(), "cannot be the same") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCoopSpecSubmit_Research(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	// Start spec
	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "test-submit",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Problem statement",
		})
	})

	// Submit research
	output, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "test-submit",
			"--phase", "research",
			"--me", "claude",
			"--root", root,
			"--body", "Research findings from claude.",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	if result["file"] != "claude-research.md" {
		t.Errorf("file = %v, want claude-research.md", result["file"])
	}
	if result["advanced"] != false {
		t.Errorf("advanced = %v, want false (only one agent submitted)", result["advanced"])
	}

	// Verify artifact file
	artifactPath := filepath.Join(fsq.SpecTopicDir(root, "test-submit"), "claude-research.md")
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(data) != "Research findings from claude." {
		t.Errorf("artifact content = %q", string(data))
	}

	// Verify state updated
	state, err := loadSpecState(root, "test-submit")
	if err != nil {
		t.Fatalf("loadSpecState: %v", err)
	}
	sub, ok := state.Submissions["claude"]["research"]
	if !ok {
		t.Fatal("claude research submission not recorded")
	}
	if sub.File != "claude-research.md" {
		t.Errorf("sub.File = %s, want claude-research.md", sub.File)
	}
}

func TestCoopSpecSubmit_BothAgents_AutoAdvance(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	// Start
	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "auto-advance",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Problem",
		})
	})

	// Claude submits research
	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "auto-advance",
			"--phase", "research",
			"--me", "claude",
			"--root", root,
			"--body", "Claude research",
		})
	})

	state, _ := loadSpecState(root, "auto-advance")
	if state.Phase != specPhaseResearch {
		t.Fatalf("phase after first submit = %s, want research", state.Phase)
	}

	// Codex submits research → should auto-advance to exchange
	output, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "auto-advance",
			"--phase", "research",
			"--me", "codex",
			"--root", root,
			"--body", "Codex research",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("codex submit: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if result["phase"] != "exchange" {
		t.Errorf("phase = %v, want exchange", result["phase"])
	}
	if result["advanced"] != true {
		t.Errorf("advanced = %v, want true", result["advanced"])
	}
}

func TestCoopSpecSubmit_InvalidPhase(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	// Start (phase=research)
	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "invalid-phase",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Problem",
		})
	})

	// Try to submit draft in research phase
	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "invalid-phase",
			"--phase", "draft",
			"--me", "claude",
			"--root", root,
			"--body", "Draft too early",
		})
	})
	if err == nil {
		t.Fatal("expected error for wrong phase")
	}
	if !strings.Contains(err.Error(), "can only submit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCoopSpecSubmit_NonParticipant(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex", "eve"})

	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "no-eve",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Problem",
		})
	})

	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "no-eve",
			"--phase", "research",
			"--me", "eve",
			"--root", root,
			"--body", "Eve tries to submit",
		})
	})
	if err == nil {
		t.Fatal("expected error for non-participant")
	}
	if !strings.Contains(err.Error(), "not a participant") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCoopSpecSubmit_ConcurrentWrites(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "concurrent",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Problem",
		})
	})

	// Two goroutines submit simultaneously
	var wg sync.WaitGroup
	errs := make([]error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = captureStdout(t, func() error {
			return runCoopSpec([]string{"submit",
				"--topic", "concurrent",
				"--phase", "research",
				"--me", "claude",
				"--root", root,
				"--body", "Claude research concurrent",
			})
		})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = captureStdout(t, func() error {
			return runCoopSpec([]string{"submit",
				"--topic", "concurrent",
				"--phase", "research",
				"--me", "codex",
				"--root", root,
				"--body", "Codex research concurrent",
			})
		})
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d error: %v", i, err)
		}
	}

	// Verify state is consistent
	state, err := loadSpecState(root, "concurrent")
	if err != nil {
		t.Fatalf("loadSpecState: %v", err)
	}

	// Both should have submitted
	if _, ok := state.Submissions["claude"]["research"]; !ok {
		t.Error("claude research not recorded")
	}
	if _, ok := state.Submissions["codex"]["research"]; !ok {
		t.Error("codex research not recorded")
	}

	// Phase should have advanced to exchange
	if state.Phase != specPhaseExchange {
		t.Errorf("phase = %s, want exchange", state.Phase)
	}
}

func TestCoopSpecStatus(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	// Start
	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "status-test",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Problem",
		})
	})

	// Text output
	textOut, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"status",
			"--topic", "status-test",
			"--root", root,
		})
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(textOut, "research") {
		t.Errorf("text output missing phase, got: %s", textOut)
	}
	if !strings.Contains(textOut, "spec/status-test") {
		t.Errorf("text output missing thread, got: %s", textOut)
	}

	// JSON output
	jsonOut, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"status",
			"--topic", "status-test",
			"--root", root,
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}

	var state specState
	if err := json.Unmarshal([]byte(jsonOut), &state); err != nil {
		t.Fatalf("parse json: %v\noutput: %s", err, jsonOut)
	}
	if state.Topic != "status-test" {
		t.Errorf("topic = %s, want status-test", state.Topic)
	}
	if state.Phase != specPhaseResearch {
		t.Errorf("phase = %s, want research", state.Phase)
	}
}

func TestCoopSpecPresent(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	// Create a spec and manually set it to done with a final spec
	topic := "present-test"
	if err := fsq.EnsureSpecDirs(root, topic); err != nil {
		t.Fatalf("EnsureSpecDirs: %v", err)
	}
	state := specState{
		Topic:       topic,
		Phase:       specPhaseDone,
		Started:     "2026-01-01T00:00:00Z",
		StartedBy:   "claude",
		Agents:      []string{"claude", "codex"},
		Thread:      "spec/" + topic,
		Submissions: map[string]map[string]specSub{},
		FinalSpec:   "final.md",
		Completed:   "2026-01-02T00:00:00Z",
	}
	if err := saveSpecState(root, topic, state); err != nil {
		t.Fatalf("saveSpecState: %v", err)
	}
	finalContent := "# Final Spec\n\nThis is the final spec.\n"
	if err := os.WriteFile(filepath.Join(fsq.SpecTopicDir(root, topic), "final.md"), []byte(finalContent), 0o600); err != nil {
		t.Fatalf("write final.md: %v", err)
	}

	// Text output
	output, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"present",
			"--topic", topic,
			"--root", root,
		})
	})
	if err != nil {
		t.Fatalf("present: %v", err)
	}
	if output != finalContent {
		t.Errorf("output = %q, want %q", output, finalContent)
	}

	// JSON output
	jsonOut, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"present",
			"--topic", topic,
			"--root", root,
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("present --json: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &result); err != nil {
		t.Fatalf("parse json: %v", err)
	}
	if result["body"] != finalContent {
		t.Errorf("body = %v", result["body"])
	}
}

func TestCoopSpecPresent_NoFinal(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	topic := "no-final"
	if err := fsq.EnsureSpecDirs(root, topic); err != nil {
		t.Fatalf("EnsureSpecDirs: %v", err)
	}
	state := specState{
		Topic:       topic,
		Phase:       specPhaseResearch,
		Started:     "2026-01-01T00:00:00Z",
		StartedBy:   "claude",
		Agents:      []string{"claude", "codex"},
		Thread:      "spec/" + topic,
		Submissions: map[string]map[string]specSub{},
	}
	if err := saveSpecState(root, topic, state); err != nil {
		t.Fatalf("saveSpecState: %v", err)
	}

	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"present",
			"--topic", topic,
			"--root", root,
		})
	})
	if err == nil {
		t.Fatal("expected error for no final spec")
	}
	if !strings.Contains(err.Error(), "no final spec") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTopicName(t *testing.T) {
	tests := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"valid simple", "auth", false},
		{"valid with hyphen", "auth-redesign", false},
		{"valid with underscore", "auth_redesign", false},
		{"valid with numbers", "feature-123", false},
		{"empty", "", true},
		{"spaces", "bad topic", true},
		{"uppercase", "BadTopic", true},
		{"path traversal dots", "../escape", true},
		{"path traversal slash", "bad/topic", true},
		{"special chars", "bad@topic", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fsq.ValidateTopicName(tt.topic)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateTopicName(%q) error = %v, wantErr = %v", tt.topic, err, tt.wantErr)
			}
		})
	}
}

func TestAdvancePhase_AllTransitions(t *testing.T) {
	agents := []string{"claude", "codex"}

	// Start at research
	state := &specState{
		Topic:  "test",
		Phase:  specPhaseResearch,
		Agents: agents,
		Submissions: map[string]map[string]specSub{
			"claude": {"research": {Submitted: "t1", File: "claude-research.md"}},
		},
	}

	// One agent submitted → no advance
	if advancePhase(state) {
		t.Error("should not advance with one research submission")
	}

	// Both submitted → advance to exchange
	state.Submissions["codex"] = map[string]specSub{"research": {Submitted: "t2", File: "codex-research.md"}}
	if !advancePhase(state) {
		t.Error("should advance to exchange")
	}
	if state.Phase != specPhaseExchange {
		t.Errorf("phase = %s, want exchange", state.Phase)
	}

	// Exchange → draft (on first draft)
	state.Submissions["claude"]["draft"] = specSub{Submitted: "t3", File: "claude-draft.md"}
	if !advancePhase(state) {
		t.Error("should advance from exchange to draft on first draft")
	}
	if state.Phase != specPhaseDraft {
		t.Errorf("phase = %s, want draft", state.Phase)
	}

	// Draft: one submitted → no advance
	if advancePhase(state) {
		t.Error("should not advance with one draft")
	}

	// Both drafts → advance to review
	state.Submissions["codex"]["draft"] = specSub{Submitted: "t4", File: "codex-draft.md"}
	if !advancePhase(state) {
		t.Error("should advance to review")
	}
	if state.Phase != specPhaseReview {
		t.Errorf("phase = %s, want review", state.Phase)
	}

	// Review: one submitted → no advance
	state.Submissions["claude"]["review"] = specSub{Submitted: "t5", File: "claude-review.md"}
	if advancePhase(state) {
		t.Error("should not advance with one review")
	}

	// Both reviews → advance to converge
	state.Submissions["codex"]["review"] = specSub{Submitted: "t6", File: "codex-review.md"}
	if !advancePhase(state) {
		t.Error("should advance to converge")
	}
	if state.Phase != specPhaseConverge {
		t.Errorf("phase = %s, want converge", state.Phase)
	}

	// Converge: no final → no advance
	if advancePhase(state) {
		t.Error("should not advance without final spec")
	}

	// Final submitted → done
	state.FinalSpec = "final.md"
	if !advancePhase(state) {
		t.Error("should advance to done")
	}
	if state.Phase != specPhaseDone {
		t.Errorf("phase = %s, want done", state.Phase)
	}
	if state.Completed == "" {
		t.Error("completed timestamp should be set")
	}
}

func TestValidSubmitPhase(t *testing.T) {
	tests := []struct {
		current string
		submit  string
		wantErr bool
	}{
		{specPhaseResearch, specPhaseResearch, false},
		{specPhaseResearch, specPhaseDraft, true},
		{specPhaseResearch, specPhaseReview, true},
		{specPhaseExchange, specPhaseDraft, false},
		{specPhaseExchange, specPhaseResearch, true},
		{specPhaseDraft, specPhaseDraft, false},
		{specPhaseDraft, specPhaseResearch, true},
		{specPhaseReview, specPhaseReview, false},
		{specPhaseReview, specPhaseDraft, true},
		{specPhaseConverge, "final", false},
		{specPhaseConverge, specPhaseReview, true},
		{specPhaseDone, "final", true},
	}
	for _, tt := range tests {
		name := tt.current + "/" + tt.submit
		t.Run(name, func(t *testing.T) {
			err := validSubmitPhase(tt.current, tt.submit)
			if (err != nil) != tt.wantErr {
				t.Errorf("validSubmitPhase(%q, %q) error = %v, wantErr = %v", tt.current, tt.submit, err, tt.wantErr)
			}
		})
	}
}

func TestCoopSpecSubmit_FullWorkflow(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	// Start
	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "full-workflow",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Problem statement",
		})
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Both submit research → advances to exchange
	for _, agent := range []string{"claude", "codex"} {
		_, err := captureStdout(t, func() error {
			return runCoopSpec([]string{"submit",
				"--topic", "full-workflow",
				"--phase", "research",
				"--me", agent,
				"--root", root,
				"--body", agent + " research findings",
			})
		})
		if err != nil {
			t.Fatalf("%s research submit: %v", agent, err)
		}
	}
	state, _ := loadSpecState(root, "full-workflow")
	if state.Phase != specPhaseExchange {
		t.Fatalf("after research: phase = %s, want exchange", state.Phase)
	}

	// Claude submits draft → exchange advances to draft
	_, err = captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "full-workflow",
			"--phase", "draft",
			"--me", "claude",
			"--root", root,
			"--body", "Claude draft spec",
		})
	})
	if err != nil {
		t.Fatalf("claude draft: %v", err)
	}
	state, _ = loadSpecState(root, "full-workflow")
	if state.Phase != specPhaseDraft {
		t.Fatalf("after first draft: phase = %s, want draft", state.Phase)
	}

	// Codex submits draft → advances to review
	_, err = captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "full-workflow",
			"--phase", "draft",
			"--me", "codex",
			"--root", root,
			"--body", "Codex draft spec",
		})
	})
	if err != nil {
		t.Fatalf("codex draft: %v", err)
	}
	state, _ = loadSpecState(root, "full-workflow")
	if state.Phase != specPhaseReview {
		t.Fatalf("after both drafts: phase = %s, want review", state.Phase)
	}

	// Both submit reviews → advances to converge
	for _, agent := range []string{"claude", "codex"} {
		_, err := captureStdout(t, func() error {
			return runCoopSpec([]string{"submit",
				"--topic", "full-workflow",
				"--phase", "review",
				"--me", agent,
				"--root", root,
				"--body", agent + " review feedback",
			})
		})
		if err != nil {
			t.Fatalf("%s review submit: %v", agent, err)
		}
	}
	state, _ = loadSpecState(root, "full-workflow")
	if state.Phase != specPhaseConverge {
		t.Fatalf("after reviews: phase = %s, want converge", state.Phase)
	}

	// Submit final → done
	_, err = captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "full-workflow",
			"--phase", "final",
			"--me", "claude",
			"--root", root,
			"--body", "# Final Spec\nConverged solution.",
		})
	})
	if err != nil {
		t.Fatalf("final submit: %v", err)
	}
	state, _ = loadSpecState(root, "full-workflow")
	if state.Phase != specPhaseDone {
		t.Fatalf("after final: phase = %s, want done", state.Phase)
	}
	if state.FinalSpec != "final.md" {
		t.Errorf("FinalSpec = %s, want final.md", state.FinalSpec)
	}
	if state.Completed == "" {
		t.Error("Completed timestamp not set")
	}

	// Present the final spec
	output, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"present",
			"--topic", "full-workflow",
			"--root", root,
		})
	})
	if err != nil {
		t.Fatalf("present: %v", err)
	}
	if !strings.Contains(output, "Final Spec") {
		t.Errorf("present output missing content: %s", output)
	}
}

// Regression test: when codex's draft advances phase to review, the NEXT STEP
// output should prompt codex to submit a review, not say "WAITING: You already
// submitted a review". See PR #28 comment from @avivsinai.
func TestCoopSpecSubmit_DraftToReviewNextStep(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	// Start + both research
	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"start", "--topic", "ns", "--partner", "codex", "--me", "claude", "--root", root, "--body", "p"})
	})
	for _, agent := range []string{"claude", "codex"} {
		_, _ = captureStdout(t, func() error {
			return runCoopSpec([]string{"submit", "--topic", "ns", "--phase", "research", "--me", agent, "--root", root, "--body", "r"})
		})
	}

	// Claude submits draft
	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"submit", "--topic", "ns", "--phase", "draft", "--me", "claude", "--root", root, "--body", "d1"})
	})

	// Codex submits draft → advances to review
	output, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"submit", "--topic", "ns", "--phase", "draft", "--me", "codex", "--root", root, "--body", "d2"})
	})
	if err != nil {
		t.Fatalf("codex draft submit: %v", err)
	}

	// Should prompt to submit review, not say "WAITING: You already submitted a review"
	if strings.Contains(output, "WAITING") {
		t.Errorf("after draft→review advance, output should not say WAITING:\n%s", output)
	}
	if !strings.Contains(output, "NEXT STEP") {
		t.Errorf("after draft→review advance, output should contain NEXT STEP:\n%s", output)
	}
	if !strings.Contains(output, "review") {
		t.Errorf("after draft→review advance, output should mention review:\n%s", output)
	}
}

func TestCoopSpecSubmit_EmptyBody(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	_, _ = captureStdout(t, func() error {
		return runCoopSpec([]string{"start",
			"--topic", "empty-body",
			"--partner", "codex",
			"--me", "claude",
			"--root", root,
			"--body", "Problem",
		})
	})

	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", "empty-body",
			"--phase", "research",
			"--me", "claude",
			"--root", root,
		})
	})
	if err == nil {
		t.Fatal("expected error for empty body")
	}
	if !strings.Contains(err.Error(), "body is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCoopSpecSubmit_DonePhase(t *testing.T) {
	root := setupSpecRoot(t, []string{"claude", "codex"})

	topic := "done-phase"
	if err := fsq.EnsureSpecDirs(root, topic); err != nil {
		t.Fatalf("EnsureSpecDirs: %v", err)
	}
	state := specState{
		Topic:       topic,
		Phase:       specPhaseDone,
		Started:     "2026-01-01T00:00:00Z",
		StartedBy:   "claude",
		Agents:      []string{"claude", "codex"},
		Thread:      "spec/" + topic,
		Submissions: map[string]map[string]specSub{},
		FinalSpec:   "final.md",
		Completed:   "2026-01-02T00:00:00Z",
	}
	if err := saveSpecState(root, topic, state); err != nil {
		t.Fatalf("saveSpecState: %v", err)
	}

	_, err := captureStdout(t, func() error {
		return runCoopSpec([]string{"submit",
			"--topic", topic,
			"--phase", "final",
			"--me", "claude",
			"--root", root,
			"--body", "Another final",
		})
	})
	if err == nil {
		t.Fatal("expected error for done phase")
	}
	if !strings.Contains(err.Error(), "already done") {
		t.Fatalf("unexpected error: %v", err)
	}
}
