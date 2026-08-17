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

func TestHasManagedFragmentRequiresBothMarkers(t *testing.T) {
	if hasManagedFragment(ManagedBegin + "\ncmd\n") {
		t.Fatal("begin marker without end is not a managed fragment")
	}
	if hasManagedFragment("cmd\n" + ManagedEnd) {
		t.Fatal("end marker without begin is not a managed fragment")
	}
}

func TestHasManagedFragmentRejectsReversedMarkers(t *testing.T) {
	reversed := ManagedEnd + "\ncmd\n" + ManagedBegin
	if hasManagedFragment(reversed) {
		t.Fatal("end-before-begin is not a managed fragment")
	}
	if _, ok := extractManagedFragment(reversed); ok {
		t.Fatal("extractManagedFragment must fail when end precedes begin")
	}
}

func TestHasManagedFragmentExtractsFirstBeginToFirstLaterEnd(t *testing.T) {
	nested := ManagedBegin + "\nouter-start\n" + ManagedBegin + "\ninner\n" + ManagedEnd + "\nouter-end\n" + ManagedEnd
	got, ok := extractManagedFragment(nested)
	if !ok {
		t.Fatal("nested markers should extract the first begin-to-end span")
	}
	want := ManagedBegin + "\nouter-start\n" + ManagedBegin + "\ninner\n" + ManagedEnd
	if got != want {
		t.Fatalf("extracted %q, want first span %q", got, want)
	}
	duplicate := managedFragment("first") + "\n" + managedFragment("second")
	got, ok = extractManagedFragment(duplicate)
	if !ok || got != managedFragment("first") {
		t.Fatalf("duplicate fragments extracted %q, want the first fragment", got)
	}
}

func TestInit_CheckModePartialManagedHooksAreNotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWorkflow(t, dir, `---
hooks:
  after_create: |
    # BEGIN AMQ MANAGED
    amq integration symphony emit --event after_create --me codex || true
    # END AMQ MANAGED
  after_run: |
    # BEGIN AMQ MANAGED
    amq integration symphony emit --event after_run --me codex || true
    # END AMQ MANAGED
  before_remove: |
    # BEGIN AMQ MANAGED
    amq integration symphony emit --event before_remove --me codex || true
    # END AMQ MANAGED
---

Prompt.
`)

	result, err := Init(InitOptions{
		WorkflowPath: path,
		Me:           "codex",
		Check:        true,
	})
	if err != nil {
		t.Fatalf("Init check: %v", err)
	}
	if result.HooksFound {
		t.Fatal("three of four managed hooks is not HooksFound")
	}
	if result.AlreadyOK {
		t.Fatal("partial managed hooks must not be AlreadyOK")
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

func TestInjectFragmentReplacesSpanWhenStrayEndPrecedesFragment(t *testing.T) {
	existing := ManagedEnd + "\nuser-before\n" + managedFragment("old-cmd") + "\nuser-after\n"
	got := injectFragment(existing, managedFragment("new-cmd"))
	want := ManagedEnd + "\nuser-before\n" + managedFragment("new-cmd") + "\nuser-after\n"
	if got != want {
		t.Fatalf("injectFragment stray-end =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "old-cmd") {
		t.Fatal("stray ManagedEnd must not keep the old managed block")
	}
	if strings.Count(got, "user-before") != 1 {
		t.Fatalf("user-before copies = %d, want 1", strings.Count(got, "user-before"))
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
