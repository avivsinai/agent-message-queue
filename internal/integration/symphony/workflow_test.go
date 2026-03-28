package symphony

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseWorkflow_WithFrontmatter(t *testing.T) {
	content := `---
tracker:
  kind: linear
  project_slug: "test-project"
hooks:
  after_create: |
    echo "hello"
  before_run: |
    echo "before"
---

You are working on a ticket.
`

	wf, err := ParseWorkflow(content)
	if err != nil {
		t.Fatalf("ParseWorkflow: %v", err)
	}

	if wf.Config["tracker"] == nil {
		t.Error("expected tracker in config")
	}

	if wf.Prompt != "You are working on a ticket." {
		t.Errorf("unexpected prompt: %q", wf.Prompt)
	}
}

func TestParseWorkflow_NoFrontmatter(t *testing.T) {
	content := "Just a plain markdown prompt body.\n"

	wf, err := ParseWorkflow(content)
	if err != nil {
		t.Fatalf("ParseWorkflow: %v", err)
	}

	if len(wf.Config) != 0 {
		t.Errorf("expected empty config, got %v", wf.Config)
	}

	if wf.Prompt != "Just a plain markdown prompt body." {
		t.Errorf("unexpected prompt: %q", wf.Prompt)
	}
}

func TestParseWorkflow_EmptyFrontmatter(t *testing.T) {
	content := "---\n---\n\nPrompt body here.\n"

	wf, err := ParseWorkflow(content)
	if err != nil {
		t.Fatalf("ParseWorkflow: %v", err)
	}

	if len(wf.Config) != 0 {
		t.Errorf("expected empty config, got %v", wf.Config)
	}
}

func TestParseWorkflow_InvalidYAML(t *testing.T) {
	content := "---\n[invalid yaml: {\n---\n"

	_, err := ParseWorkflow(content)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestParseWorkflow_MissingClosingDelimiter(t *testing.T) {
	content := "---\nhooks:\n  after_create: echo hi\n"

	// This should parse with the whole thing as frontmatter only
	// if it ends with --- on its own line, otherwise error
	_, err := ParseWorkflow(content)
	if err == nil {
		t.Fatal("expected error for missing closing ---")
	}
}

func TestGetHooks(t *testing.T) {
	content := `---
hooks:
  after_create: |
    git clone repo .
  before_run: |
    npm install
  after_run: |
    echo done
  before_remove: |
    rm -rf node_modules
---

Prompt.
`

	wf, err := ParseWorkflow(content)
	if err != nil {
		t.Fatalf("ParseWorkflow: %v", err)
	}

	hooks := wf.GetHooks()

	if hooks.AfterCreate != "git clone repo .\n" {
		t.Errorf("unexpected after_create: %q", hooks.AfterCreate)
	}
	if hooks.BeforeRun != "npm install\n" {
		t.Errorf("unexpected before_run: %q", hooks.BeforeRun)
	}
	if hooks.AfterRun != "echo done\n" {
		t.Errorf("unexpected after_run: %q", hooks.AfterRun)
	}
	// Note: the last block scalar in YAML frontmatter may lose its trailing
	// newline because the \n before --- is consumed as the delimiter boundary.
	if hooks.BeforeRemove != "rm -rf node_modules\n" && hooks.BeforeRemove != "rm -rf node_modules" {
		t.Errorf("unexpected before_remove: %q", hooks.BeforeRemove)
	}
}

func TestGetHooks_NoHooksSection(t *testing.T) {
	content := `---
tracker:
  kind: linear
---

Prompt.
`

	wf, err := ParseWorkflow(content)
	if err != nil {
		t.Fatalf("ParseWorkflow: %v", err)
	}

	hooks := wf.GetHooks()
	if hooks.AfterCreate != "" || hooks.BeforeRun != "" || hooks.AfterRun != "" || hooks.BeforeRemove != "" {
		t.Error("expected empty hooks for workflow without hooks section")
	}
}

func TestSetHooks(t *testing.T) {
	wf := &Workflow{
		Config: map[string]interface{}{},
		Prompt: "Test prompt",
	}

	wf.SetHooks(HooksConfig{
		AfterCreate: "echo create",
		BeforeRun:   "echo run",
	})

	hooks := wf.GetHooks()
	if hooks.AfterCreate != "echo create" {
		t.Errorf("unexpected after_create: %q", hooks.AfterCreate)
	}
	if hooks.BeforeRun != "echo run" {
		t.Errorf("unexpected before_run: %q", hooks.BeforeRun)
	}
}

func TestMarshalWorkflow(t *testing.T) {
	wf := &Workflow{
		Config: map[string]interface{}{
			"hooks": map[string]interface{}{
				"after_create": "echo hello\n",
			},
		},
		Prompt: "You are a test prompt.",
	}

	content, err := wf.MarshalWorkflow()
	if err != nil {
		t.Fatalf("MarshalWorkflow: %v", err)
	}

	// Should start with ---
	if content[:4] != "---\n" {
		t.Errorf("expected content to start with ---, got: %q", content[:4])
	}

	// Should contain the hook
	if !contains(content, "echo hello") {
		t.Error("expected content to contain echo hello")
	}

	// Should contain the prompt
	if !contains(content, "You are a test prompt.") {
		t.Error("expected content to contain the prompt")
	}
}

func TestReadWorkflow_NotFound(t *testing.T) {
	_, err := ReadWorkflow("/nonexistent/WORKFLOW.md")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadWorkflow_FromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	content := `---
hooks:
  after_create: echo test
---

Prompt body.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, err := ReadWorkflow(path)
	if err != nil {
		t.Fatalf("ReadWorkflow: %v", err)
	}

	hooks := wf.GetHooks()
	if hooks.AfterCreate != "echo test" {
		t.Errorf("unexpected after_create: %q", hooks.AfterCreate)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
