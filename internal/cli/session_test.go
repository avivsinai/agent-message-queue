package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSessionCreateProvisionsRosterMailboxes(t *testing.T) {
	base := makeSessionBase(t)
	writeKnownAgentsConfig(t, base, []string{"claude", "codex"})

	output, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", base, "--json", "feature-x"})
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var result sessionCreateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode create json: %v (%s)", err, output)
	}
	if result.Name != "feature-x" {
		t.Fatalf("name = %q, want feature-x", result.Name)
	}
	for _, agent := range []string{"claude", "codex", reservedHumanHandle} {
		if _, err := os.Stat(filepath.Join(result.Path, "agents", agent, "inbox", "new")); err != nil {
			t.Fatalf("mailbox %s: %v", agent, err)
		}
	}
}

func TestSessionCreatePrefersLaunchRoster(t *testing.T) {
	base := makeSessionBase(t)
	writeKnownAgentsConfig(t, base, []string{"alice"})
	project := isolateSessionProject(t)
	writeProjectAmqrc(t, project)
	writeLaunchRoster(t, project, `{"schema":1,"agents":[{"handle":"claude","command":["claude"]}]}`)
	t.Chdir(project)

	output, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", base, "--json", "squad"})
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var result sessionCreateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode: %v (%s)", err, output)
	}
	if !reflect.DeepEqual(result.Agents, []string{"claude"}) {
		t.Fatalf("agents = %#v, want [claude] from launch.json", result.Agents)
	}
	if _, err := os.Stat(filepath.Join(result.Path, "agents", "claude", "inbox", "new")); err != nil {
		t.Fatalf("claude mailbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.Path, "agents", "alice")); !os.IsNotExist(err) {
		t.Fatalf("alice mailbox should not be created from base config: %v", err)
	}
}

func TestSessionListReportsCanonicalAndLegacy(t *testing.T) {
	base := makeSessionBase(t)
	if _, err := provisionCoopSession(base, "collab", []string{"claude"}, "", ""); err != nil {
		t.Fatalf("provision collab: %v", err)
	}
	legacy := filepath.Join(base, "foo.bar")
	if err := fsq.EnsureRootDirs(legacy); err != nil {
		t.Fatalf("EnsureRootDirs legacy: %v", err)
	}
	if err := fsq.EnsureAgentDirs(legacy, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "notes.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	output, err := captureEnvStdout(t, func() error {
		return runSessionList([]string{"--root", base, "--json"})
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var result sessionListResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode: %v (%s)", err, output)
	}

	byName := map[string]sessionListEntry{}
	for _, item := range result.Sessions {
		byName[item.Name] = item
	}
	collab, ok := byName["collab"]
	if !ok || collab.Kind != sessionKindCanonical {
		t.Fatalf("collab = %#v, want canonical", collab)
	}
	legacyItem, ok := byName["foo.bar"]
	if !ok || legacyItem.Kind != sessionKindLegacy {
		t.Fatalf("foo.bar = %#v, want legacy_name", legacyItem)
	}
	if legacyItem.Hint != "amq list --root "+legacyItem.Path {
		t.Fatalf("legacy hint = %q", legacyItem.Hint)
	}

	foundFile := false
	for _, skipped := range result.Skipped {
		if skipped.Name == "notes.txt" && skipped.Reason == "not_a_directory" {
			foundFile = true
		}
	}
	if !foundFile {
		t.Fatalf("notes.txt should be skipped in json: %#v", result.Skipped)
	}

	text, err := captureEnvStdout(t, func() error {
		return runSessionList([]string{"--root", base})
	})
	if err != nil {
		t.Fatalf("text list: %v", err)
	}
	if strings.Contains(text, "notes.txt") {
		t.Fatalf("text list leaked hostile file: %s", text)
	}
}

func makeSessionBase(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	return base
}

func isolateSessionProject(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envRoot, "")
	t.Setenv(envBaseRoot, "")
	t.Setenv(envSession, "")
	t.Setenv(envGlobalRoot, "")
	return t.TempDir()
}

func writeProjectAmqrc(t *testing.T, project string) {
	t.Helper()
	data := []byte(`{"root":".agent-mail"}`)
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), data, 0o600); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}
}

func writeLaunchRoster(t *testing.T, project, body string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(project, ".amq"), 0o700); err != nil {
		t.Fatalf("mkdir .amq: %v", err)
	}
	if err := os.WriteFile(filepath.Join(project, ".amq", "launch.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write launch.json: %v", err)
	}
}
