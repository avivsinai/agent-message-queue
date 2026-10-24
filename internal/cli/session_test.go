package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/execidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
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
	// agent-message-queue-t97.5: a launch.json handle missing from the base
	// roster was provisioned but classified unregistered in the new session.
	if status, diags := AgentRegistration(result.Path, "claude"); status != string(registrationDeclared) {
		t.Fatalf("claude registration = %s %v, want declared from launch.json", status, diags)
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

// Slice 4 of the fleet board: session list --json names, per agent, the
// harness sessions exact AMQ records tie to it.
func TestSessionListShowsRecordedHarnessSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	base := makeSessionBase(t)
	session, err := provisionCoopSession(base, "collab", []string{"alpha", "beta"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	rootID, baseID := treeIdentityTokens(session, base)
	if rootID == "" {
		t.Fatal("no root identity token")
	}

	const launchID, firstThread, secondThread, claudeID = "11111111-1111-4111-8111-111111111111",
		"22222222-2222-7222-8222-222222222222", "33333333-3333-7333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	identity, err := fsq.SnapshotDeliveryRoot(session)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(session, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lease, err := launch.AcquireLease(root, launchID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := launch.WriteConversation(root, lease, launch.ConversationRecord{
		Version: launch.ConversationVersion, Handle: "alpha", State: launch.CaptureReady, LaunchNonce: launchID,
		Identity: launch.ConversationIdentity{Provider: launch.ClaudeProvider, ID: launchID},
		ExecutionEvidence: &launch.ConversationExecutionEvidence{
			Backend: "tmux", Profile: "tmux/darwin/v1", Outcome: launch.OutcomeCreated, LaunchNonce: launchID, ConversationID: launchID,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	for _, thread := range []string{firstThread, secondThread} {
		if err := codexidentity.Publish(codexidentity.Record{
			Thread: thread, CodexHome: filepath.Join(home, ".codex"), Root: session, BaseRoot: base, Session: "collab",
			Me: "beta", RootID: rootID, BaseRootID: baseID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Claude writes procStart in UTC; a local zone off UTC must not shift the
	// join (seen live: every pid missed by the local offset before the fix).
	local := time.Local
	time.Local = time.FixedZone("UTC+3", 3*60*60)
	t.Cleanup(func() { time.Local = local })
	start := time.Date(2026, 10, 9, 10, 10, 50, 0, time.UTC)
	if err := execidentity.Publish(execidentity.Record{
		Harness: launch.ClaudeProvider, Holder: launch.HolderIdentity{PID: 4242, ProcessStart: "start", BootID: "boot"},
		StartUnix: start.Unix(), Root: session, BaseRoot: base, Session: "collab", Me: "alpha", RootID: rootID, BaseRootID: baseID,
	}); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeState := fmt.Sprintf(`{"pid":4242,"sessionId":%q,"procStart":%q,"cwd":"/elsewhere"}`, claudeID, start.UTC().Format("Mon Jan _2 15:04:05 2006"))
	if err := os.WriteFile(filepath.Join(sessions, "4242.json"), []byte(claudeState), 0o600); err != nil {
		t.Fatal(err)
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
	want := []sessionConversation{
		{Handle: "alpha", Provider: "claude", ID: claudeID, State: "ready", Source: "exec-pid"},
		{Handle: "alpha", Provider: "claude", ID: launchID, State: "ready", Source: "launch"},
		{Handle: "beta", Provider: "codex", ID: firstThread, State: "revoked", Source: "codex-thread"},
		{Handle: "beta", Provider: "codex", ID: secondThread, State: "ready", Source: "codex-thread"},
	}
	if len(result.Sessions) != 1 || !reflect.DeepEqual(result.Sessions[0].Conversations, want) {
		t.Fatalf("conversations = %#v, want %#v", result.Sessions, want)
	}
}

// #1192 review: two repeat-publish cases of the Codex thread store.
func TestSessionListKeepsCodexBindingHistory(t *testing.T) {
	const threadA, threadB = "55555555-5555-7555-8555-555555555555", "66666666-6666-7666-8666-666666666666"
	setup := func(t *testing.T) (string, func(thread, me string)) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		base := makeSessionBase(t)
		session, err := provisionCoopSession(base, "collab", []string{"alpha", "beta"}, "", "")
		if err != nil {
			t.Fatal(err)
		}
		rootID, baseID := treeIdentityTokens(session, base)
		publish := func(thread, me string) {
			if err := codexidentity.Publish(codexidentity.Record{
				Thread: thread, CodexHome: filepath.Join(home, ".codex"), Root: session, BaseRoot: base, Session: "collab",
				Me: me, RootID: rootID, BaseRootID: baseID,
			}); err != nil {
				t.Fatal(err)
			}
		}
		return base, publish
	}
	list := func(t *testing.T, base string) []sessionConversation {
		output, err := captureEnvStdout(t, func() error { return runSessionList([]string{"--root", base, "--json"}) })
		if err != nil {
			t.Fatal(err)
		}
		var result sessionListResult
		if err := json.Unmarshal([]byte(output), &result); err != nil || len(result.Sessions) != 1 {
			t.Fatalf("decode: %v (%s)", err, output)
		}
		return result.Sessions[0].Conversations
	}

	t.Run("resuming the successor does not revive an expired binding", func(t *testing.T) {
		base, publish := setup(t)
		publish(threadA, "beta")
		publish(threadB, "beta")
		ageCodexStore(t, 10*24*time.Hour)
		publish(threadB, "beta")
		want := []sessionConversation{{Handle: "beta", Provider: "codex", ID: threadB, State: "ready", Source: "codex-thread"}}
		if got := list(t, base); !reflect.DeepEqual(got, want) {
			t.Fatalf("conversations = %#v, want %#v", got, want)
		}
	})

	t.Run("a binding recorded before the history stays listed when replaced", func(t *testing.T) {
		base, publish := setup(t)
		publish(threadA, "beta")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		// A store written before binding events existed has none.
		if err := os.RemoveAll(filepath.Join(home, ".amq", "codex-threads", "bindings")); err != nil {
			t.Fatal(err)
		}
		publish(threadB, "beta")
		want := []sessionConversation{
			{Handle: "beta", Provider: "codex", ID: threadA, State: "revoked", Source: "codex-thread"},
			{Handle: "beta", Provider: "codex", ID: threadB, State: "ready", Source: "codex-thread"},
		}
		if got := list(t, base); !reflect.DeepEqual(got, want) {
			t.Fatalf("conversations = %#v, want %#v", got, want)
		}
	})

	t.Run("giving a thread to another handle keeps the earlier binding", func(t *testing.T) {
		base, publish := setup(t)
		publish(threadA, "alpha")
		publish(threadA, "beta")
		want := []sessionConversation{
			{Handle: "alpha", Provider: "codex", ID: threadA, State: "revoked", Source: "codex-thread"},
			{Handle: "beta", Provider: "codex", ID: threadA, State: "ready", Source: "codex-thread"},
		}
		if got := list(t, base); !reflect.DeepEqual(got, want) {
			t.Fatalf("conversations = %#v, want %#v", got, want)
		}
	})
}

// ageCodexStore moves every write in the Codex thread store age into the
// past: file times, and the binding time inside each binding event.
func ageCodexStore(t *testing.T, age time.Duration) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(filepath.Join(home, ".amq", "codex-threads"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		if filepath.Base(filepath.Dir(path)) == "bindings" {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var event map[string]any
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			bound, err := time.Parse(time.RFC3339Nano, event["bound"].(string))
			if err != nil {
				return err
			}
			event["bound"] = bound.Add(-age).Format(time.RFC3339Nano)
			if raw, err = json.Marshal(event); err != nil {
				return err
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				return err
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		return os.Chtimes(path, info.ModTime().Add(-age), info.ModTime().Add(-age))
	})
	if err != nil {
		t.Fatal(err)
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
