package symphony

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestWorkflow(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInit_BasicInjection(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWorkflow(t, dir, `---
hooks:
  after_create: |
    git clone repo .
---

Prompt.
`)

	result, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Root:         "/tmp/test-root",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if !result.Created {
		t.Error("expected Created=true")
	}

	// Read back and verify
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Should have managed markers
	if !strings.Contains(content, ManagedBegin) {
		t.Error("expected managed begin marker")
	}
	if !strings.Contains(content, ManagedEnd) {
		t.Error("expected managed end marker")
	}

	// Should have emit commands for all events
	for _, event := range HookEvents {
		expected := "amq integration symphony emit --event " + event + " --me codex --root /tmp/test-root || true"
		if !strings.Contains(content, expected) {
			t.Errorf("expected hook line for %s: %q", event, expected)
		}
	}

	// Should preserve existing user content
	if !strings.Contains(content, "git clone repo .") {
		t.Error("expected existing user hook content to be preserved")
	}
}

func TestInit_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWorkflow(t, dir, `---
hooks:
  after_create: echo original
---

Prompt.
`)

	// First init
	_, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Root:         "/tmp/root",
	})
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Second init without --force
	result, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Root:         "/tmp/root",
	})
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}

	if !result.AlreadyOK {
		t.Error("expected AlreadyOK=true for second run")
	}

	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Error("second init changed file content without --force")
	}
}

func TestInit_ForceRewrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWorkflow(t, dir, `---
hooks:
  after_create: echo original
---

Prompt.
`)

	// First init with root A
	_, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Root:         "/tmp/root-a",
	})
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	// Force rewrite with root B
	result, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Root:         "/tmp/root-b",
		Force:        true,
	})
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}

	if !result.Updated {
		t.Error("expected Updated=true for force rewrite")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), "/tmp/root-b") {
		t.Error("expected new root in hook lines")
	}
	if strings.Contains(string(data), "/tmp/root-a") {
		t.Error("expected old root to be replaced")
	}
}

func TestInit_RefreshesStaleManagedFragmentWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWorkflow(t, dir, `---
hooks:
  after_create: echo original
---

Prompt.
`)

	_, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Root:         "/tmp/root-a",
	})
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	result, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Root:         "/tmp/root-b",
	})
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}

	if !result.Updated {
		t.Fatal("expected Updated=true when managed fragment changed")
	}
	if result.AlreadyOK {
		t.Fatal("expected AlreadyOK=false when managed fragment was stale")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "/tmp/root-b") {
		t.Fatal("expected new root in rewritten hook fragment")
	}
	if strings.Contains(content, "/tmp/root-a") {
		t.Fatal("expected stale root to be replaced")
	}
}

func TestInit_CheckMode(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWorkflow(t, dir, `---
hooks:
  after_create: echo test
---

Prompt.
`)

	// Check before init
	result, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Check:        true,
	})
	if err != nil {
		t.Fatalf("Init check: %v", err)
	}

	if !result.CheckOnly {
		t.Error("expected CheckOnly=true")
	}
	if result.HooksFound {
		t.Error("expected HooksFound=false before init")
	}

	// Now actually init
	_, err = Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Check after init
	result, err = Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Check:        true,
	})
	if err != nil {
		t.Fatalf("Init check after: %v", err)
	}

	if !result.HooksFound {
		t.Error("expected HooksFound=true after init")
	}
}

func TestInit_NoExistingHooks(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWorkflow(t, dir, `---
tracker:
  kind: linear
---

Prompt body.
`)

	result, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if !result.Created {
		t.Error("expected Created=true")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Should have all four hooks with managed fragments
	for _, event := range HookEvents {
		if !strings.Contains(content, "--event "+event) {
			t.Errorf("expected hook for event %s", event)
		}
	}
}

func TestInit_PreservesPromptBody(t *testing.T) {
	dir := t.TempDir()
	prompt := "You are working on a Linear ticket.\nDo the work carefully."
	path := writeTestWorkflow(t, dir, "---\nhooks:\n  after_create: echo test\n---\n\n"+prompt+"\n")

	_, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), prompt) {
		t.Error("expected prompt body to be preserved")
	}
}

func TestInit_NoRootPin(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWorkflow(t, dir, "---\nhooks:\n  after_create: echo test\n---\n\nPrompt.\n")

	_, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Root:         "", // no root to pin
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Should NOT contain --root flag
	if strings.Contains(string(data), "--root") {
		t.Error("expected no --root when root is empty")
	}
}

func TestGenerateHookLine_QuotesRootWithSpaces(t *testing.T) {
	line := generateHookLine("after_run", "codex", "/tmp/root with spaces")
	if !strings.Contains(line, "--root '/tmp/root with spaces'") {
		t.Fatalf("expected shell-quoted root, got %q", line)
	}
}

func TestInit_MissingWorkflow(t *testing.T) {
	_, err := Init(InitOptions{
		WorkflowPath: "/nonexistent/WORKFLOW.md",
		Me:           "codex",
	})
	if err == nil {
		t.Fatal("expected error for missing WORKFLOW.md")
	}
}

func TestInjectFragment_EmptyExisting(t *testing.T) {
	fragment := managedFragment("amq integration symphony emit --event after_create --me codex || true")
	result := injectFragment("", fragment)

	if !strings.Contains(result, ManagedBegin) {
		t.Error("expected managed begin marker")
	}
	if !strings.Contains(result, ManagedEnd) {
		t.Error("expected managed end marker")
	}
}

func TestInjectFragment_PreservesExisting(t *testing.T) {
	existing := "git clone repo .\nnpm install\n"
	fragment := managedFragment("amq integration symphony emit --event after_create --me codex || true")
	result := injectFragment(existing, fragment)

	if !strings.Contains(result, "git clone repo .") {
		t.Error("expected existing content preserved")
	}
	if !strings.Contains(result, ManagedBegin) {
		t.Error("expected managed begin marker")
	}
}

func TestInjectFragment_ReplacesExisting(t *testing.T) {
	existing := "git clone repo .\n" +
		ManagedBegin + "\n" +
		"old emit line || true\n" +
		ManagedEnd + "\n"

	fragment := managedFragment("new emit line || true")
	result := injectFragment(existing, fragment)

	if strings.Contains(result, "old emit line") {
		t.Error("expected old fragment to be replaced")
	}
	if !strings.Contains(result, "new emit line") {
		t.Error("expected new fragment content")
	}
	if !strings.Contains(result, "git clone repo .") {
		t.Error("expected user content preserved")
	}

	// Should have exactly one pair of markers
	beginCount := strings.Count(result, ManagedBegin)
	endCount := strings.Count(result, ManagedEnd)
	if beginCount != 1 || endCount != 1 {
		t.Errorf("expected 1 managed block, got begin=%d end=%d", beginCount, endCount)
	}
}
