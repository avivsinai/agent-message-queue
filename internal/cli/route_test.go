package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRouteExplainSameSession(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "collab")
	ensureRouteAgents(t, sourceRoot, "alice", "bob")

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
	)

	if !result.Routable {
		t.Fatalf("expected routable route, got error: %s", result.Error)
	}
	expectSamePath(t, result.SourceRoot, sourceRoot)
	expectSamePath(t, result.DeliveryRoot, sourceRoot)
	if result.SourceSession != "collab" {
		t.Errorf("source_session = %q, want collab", result.SourceSession)
	}
	if result.TargetSession != "collab" {
		t.Errorf("target_session = %q, want collab", result.TargetSession)
	}
	wantArgv := []string{"amq", "send", "--root", sourceRoot, "--me", "alice", "--to", "bob"}
	expectStringSlice(t, result.Argv, wantArgv)
}

func TestRouteExplainCrossSession(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "dev")
	targetRoot := filepath.Join(baseRoot, "qa")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, targetRoot, "bob")

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
		"--session", "qa",
	)

	if !result.Routable {
		t.Fatalf("expected routable route, got error: %s", result.Error)
	}
	if result.DeliveryRoot != targetRoot {
		t.Errorf("delivery_root = %q, want %q", result.DeliveryRoot, targetRoot)
	}
	if result.SourceSession != "dev" {
		t.Errorf("source_session = %q, want dev", result.SourceSession)
	}
	if result.TargetSession != "qa" {
		t.Errorf("target_session = %q, want qa", result.TargetSession)
	}
	wantArgv := []string{"amq", "send", "--root", sourceRoot, "--me", "alice", "--to", "bob", "--session", "qa"}
	expectStringSlice(t, result.Argv, wantArgv)
}

func TestRouteExplainCrossProjectMirrorsSourceSession(t *testing.T) {
	srcProjectDir := filepath.Join(t.TempDir(), "src-project")
	sourceRoot := filepath.Join(srcProjectDir, ".agent-mail", "collab")
	peerProjectDir := filepath.Join(t.TempDir(), "peer-project")
	peerBaseRoot := filepath.Join(peerProjectDir, ".agent-mail")
	peerSessionRoot := filepath.Join(peerBaseRoot, "collab")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, peerSessionRoot, "bob")
	writeRouteAmqrc(t, srcProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "src-project",
		"peers": map[string]string{
			"peer-project": peerBaseRoot,
		},
	})

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
		"--project", "peer-project",
	)

	if !result.Routable {
		t.Fatalf("expected routable route, got error: %s", result.Error)
	}
	if result.DeliveryRoot != peerSessionRoot {
		t.Errorf("delivery_root = %q, want %q", result.DeliveryRoot, peerSessionRoot)
	}
	if result.SourceProject != "src-project" {
		t.Errorf("source_project = %q, want src-project", result.SourceProject)
	}
	if result.TargetProject != "peer-project" {
		t.Errorf("target_project = %q, want peer-project", result.TargetProject)
	}
	if result.TargetSession != "collab" {
		t.Errorf("target_session = %q, want collab", result.TargetSession)
	}
	wantArgv := []string{"amq", "send", "--root", sourceRoot, "--me", "alice", "--to", "bob", "--project", "peer-project", "--session", "collab"}
	expectStringSlice(t, result.Argv, wantArgv)
}

func TestRouteExplainCrossProjectExplicitSession(t *testing.T) {
	srcProjectDir := filepath.Join(t.TempDir(), "src-project")
	sourceRoot := filepath.Join(srcProjectDir, ".agent-mail", "cto")
	peerProjectDir := filepath.Join(t.TempDir(), "peer-project")
	peerBaseRoot := filepath.Join(peerProjectDir, ".agent-mail")
	peerSessionRoot := filepath.Join(peerBaseRoot, "qa")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, peerSessionRoot, "bob")
	writeRouteAmqrc(t, srcProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "src-project",
		"peers": map[string]string{
			"peer-project": peerBaseRoot,
		},
	})

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
		"--project", "peer-project",
		"--session", "qa",
	)

	if !result.Routable {
		t.Fatalf("expected routable route, got error: %s", result.Error)
	}
	if result.DeliveryRoot != peerSessionRoot {
		t.Errorf("delivery_root = %q, want %q", result.DeliveryRoot, peerSessionRoot)
	}
	if result.SourceSession != "cto" {
		t.Errorf("source_session = %q, want cto", result.SourceSession)
	}
	if result.TargetSession != "qa" {
		t.Errorf("target_session = %q, want qa", result.TargetSession)
	}
	wantArgv := []string{"amq", "send", "--root", sourceRoot, "--me", "alice", "--to", "bob", "--project", "peer-project", "--session", "qa"}
	expectStringSlice(t, result.Argv, wantArgv)
}

func TestRouteExplainMissingPeerIsNonRoutable(t *testing.T) {
	srcProjectDir := filepath.Join(t.TempDir(), "src-project")
	sourceRoot := filepath.Join(srcProjectDir, ".agent-mail", "collab")
	ensureRouteAgents(t, sourceRoot, "alice")
	writeRouteAmqrc(t, srcProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "src-project",
	})

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
		"--project", "peer-project",
	)

	if result.Routable {
		t.Fatalf("expected non-routable result")
	}
	if len(result.Argv) != 0 {
		t.Fatalf("expected empty argv for non-routable result, got %v", result.Argv)
	}
	if result.DisplayCommand != "" {
		t.Fatalf("expected empty display_command for non-routable result, got %q", result.DisplayCommand)
	}
	if result.Error == "" {
		t.Fatalf("expected error for non-routable result")
	}
	if result.SourceRoot != sourceRoot {
		t.Errorf("source_root = %q, want %q", result.SourceRoot, sourceRoot)
	}
	if result.TargetProject != "peer-project" {
		t.Errorf("target_project = %q, want peer-project", result.TargetProject)
	}
}

func TestRouteExplainFromCWD(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project-a")
	sourceRoot := filepath.Join(projectDir, ".agent-mail")
	ensureRouteAgents(t, sourceRoot, "alice", "bob")
	writeRouteAmqrc(t, projectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "project-a",
	})

	outsideDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv(envRoot, "")
	t.Setenv(envGlobalRoot, "")
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatalf("chdir outside: %v", err)
	}
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	result := runRouteExplainJSONForTest(t,
		"--from-cwd", projectDir,
		"--me", "alice",
		"--to", "bob",
	)

	if !result.Routable {
		t.Fatalf("expected routable route, got error: %s", result.Error)
	}
	expectSamePath(t, result.SourceRoot, sourceRoot)
	expectSamePath(t, result.DeliveryRoot, sourceRoot)
	if result.SourceProject != "project-a" {
		t.Errorf("source_project = %q, want project-a", result.SourceProject)
	}
}

func TestRouteExplainJSONV1Fields(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "collab")
	ensureRouteAgents(t, sourceRoot, "alice", "bob")

	routable := runRouteExplainRawJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
	)
	required := []string{
		"schema_version",
		"routable",
		"argv",
		"display_command",
		"source_root",
		"delivery_root",
		"source_project",
		"target_project",
		"source_session",
		"target_session",
	}
	for _, key := range required {
		if _, ok := routable[key]; !ok {
			t.Errorf("expected routable JSON field %q to be present", key)
		}
	}
	if _, ok := routable["error"]; ok {
		t.Errorf("expected routable JSON output to omit error, got %s", routable["error"])
	}

	srcProjectDir := filepath.Join(t.TempDir(), "src-project")
	nonRoutableRoot := filepath.Join(srcProjectDir, ".agent-mail", "collab")
	ensureRouteAgents(t, nonRoutableRoot, "alice")
	writeRouteAmqrc(t, srcProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "src-project",
	})
	nonRoutable := runRouteExplainRawJSONForTest(t,
		"--from-root", nonRoutableRoot,
		"--me", "alice",
		"--to", "bob",
		"--project", "peer-project",
	)
	for _, key := range required {
		if _, ok := nonRoutable[key]; !ok {
			t.Errorf("expected non-routable JSON field %q to be present", key)
		}
	}
	if _, ok := nonRoutable["error"]; !ok {
		t.Errorf("expected non-routable JSON output to include error")
	}
}

func runRouteExplainJSONForTest(t *testing.T, args ...string) routeExplainResult {
	t.Helper()

	outArgs := append([]string{"--json"}, args...)
	output, err := captureEnvStdout(t, func() error {
		return runRouteExplain(outArgs)
	})
	if err != nil {
		t.Fatalf("runRouteExplain: %v", err)
	}

	var result routeExplainResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal route explain output: %v, output was: %s", err, output)
	}
	return result
}

func runRouteExplainRawJSONForTest(t *testing.T, args ...string) map[string]json.RawMessage {
	t.Helper()

	outArgs := append([]string{"--json"}, args...)
	output, err := captureEnvStdout(t, func() error {
		return runRouteExplain(outArgs)
	})
	if err != nil {
		t.Fatalf("runRouteExplain: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal raw route explain output: %v, output was: %s", err, output)
	}
	return result
}

func ensureRouteAgents(t *testing.T, root string, agents ...string) {
	t.Helper()

	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs(%q): %v", root, err)
	}
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%q, %q): %v", root, agent, err)
		}
	}
}

func writeRouteAmqrc(t *testing.T, dir string, value map[string]any) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal amqrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".amqrc"), data, 0o600); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}
}

func expectStringSlice(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("slice length = %d, want %d; got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slice[%d] = %q, want %q; got %v", i, got[i], want[i], got)
		}
	}
}
