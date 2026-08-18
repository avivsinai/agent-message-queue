package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestSessionCreateUsesSharedCreationLock(t *testing.T) {
	base := makeSessionBase(t)
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	entered := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- launch.WithSessionCreationLock(root, "auth", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	created := make(chan error, 1)
	go func() {
		_, createErr := provisionNewNamedSession(base, "auth", []string{"claude"})
		created <- createErr
	}()
	select {
	case err := <-created:
		t.Fatalf("session create bypassed shared creation lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
}

func TestSessionCreateExistingFailsLoudly(t *testing.T) {
	base := makeSessionBase(t)
	writeKnownAgentsConfig(t, base, []string{"claude", "codex"})

	if _, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", base, "auth"})
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", base, "auth"})
	})
	if err == nil {
		t.Fatal("creating an existing session succeeded")
	}
	if GetExitCode(err) != ExitError {
		t.Fatalf("exit code = %d, want %d: %v", GetExitCode(err), ExitError, err)
	}
	var exists *sessionExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("error type = %T (%v), want *sessionExistsError", err, err)
	}
	if exists.Name != "auth" {
		t.Fatalf("exists.Name = %q, want auth", exists.Name)
	}
}

func TestSessionCreateInvalidNameWritesNothing(t *testing.T) {
	base := makeSessionBase(t)
	before := dirNames(t, base)

	_, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", base, "Feature-X"})
	})
	if err == nil {
		t.Fatal("invalid session name succeeded")
	}
	if GetExitCode(err) != ExitUsage {
		t.Fatalf("exit code = %d, want %d: %v", GetExitCode(err), ExitUsage, err)
	}
	after := dirNames(t, base)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("directory listing changed: before %#v after %#v", before, after)
	}
}

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

func TestSessionCreateTrailingJSONFlag(t *testing.T) {
	base := makeSessionBase(t)
	output, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"feat-x", "--json", "--root", base})
	})
	if err != nil {
		t.Fatalf("create with trailing --json: %v", err)
	}
	var result sessionCreateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("expected json output: %v (%s)", err, output)
	}
	if result.Name != "feat-x" {
		t.Fatalf("name = %q, want feat-x", result.Name)
	}
}

func TestSessionCreateMaterializesBaseWhenAmqrcPresent(t *testing.T) {
	project := isolateSessionProject(t)
	writeProjectAmqrc(t, project)
	t.Chdir(project)

	if _, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"auth"})
	}); err != nil {
		t.Fatalf("create in configured repo: %v", err)
	}
	base := filepath.Join(project, defaultCoopRoot)
	if _, err := os.Stat(filepath.Join(base, "meta")); err != nil {
		t.Fatalf("base tree was not materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "auth", "agents")); err != nil {
		t.Fatalf("session was not created: %v", err)
	}
}

func TestSessionCreateUnconfiguredRepoIsNotFound(t *testing.T) {
	project := isolateSessionProject(t)
	t.Chdir(project)
	missing := filepath.Join(project, defaultCoopRoot)

	_, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", missing, "auth"})
	})
	if err == nil {
		t.Fatal("unconfigured create succeeded")
	}
	if GetExitCode(err) != ExitNotFound {
		t.Fatalf("exit code = %d, want %d: %v", GetExitCode(err), ExitNotFound, err)
	}
	if !strings.Contains(err.Error(), "amq setup") || !strings.Contains(err.Error(), "amq coop init") {
		t.Fatalf("error = %v, want setup/coop init remedy", err)
	}
	if _, statErr := os.Lstat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("unconfigured create wrote %s: %v", missing, statErr)
	}
}

func TestSessionCreateLaunchRosterFromAmqrcDirNotCwd(t *testing.T) {
	base := makeSessionBase(t)
	writeKnownAgentsConfig(t, base, []string{"alice"})
	project := isolateSessionProject(t)
	writeProjectAmqrc(t, project)
	writeLaunchRoster(t, project, `{"schema":1,"agents":[{"handle":"claude","command":["claude"]}]}`)
	nested := filepath.Join(project, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	t.Chdir(nested)

	output, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", base, "--json", "squad"})
	})
	if err != nil {
		t.Fatalf("create from subdirectory: %v", err)
	}
	var result sessionCreateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode: %v (%s)", err, output)
	}
	if !reflect.DeepEqual(result.Agents, []string{"claude"}) {
		t.Fatalf("agents = %#v, want [claude] from project launch.json", result.Agents)
	}
}

func TestSessionCreateRaceIsLoud(t *testing.T) {
	base := makeSessionBase(t)
	old := sessionCreateBeforeExclusive
	sessionCreateBeforeExclusive = func(root, name string) {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatalf("pre-create: %v", err)
		}
	}
	t.Cleanup(func() { sessionCreateBeforeExclusive = old })

	_, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", base, "auth"})
	})
	if err == nil {
		t.Fatal("racing create succeeded silently")
	}
	if GetExitCode(err) != ExitError {
		t.Fatalf("exit code = %d, want %d: %v", GetExitCode(err), ExitError, err)
	}
	var exists *sessionExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("error = %T (%v), want *sessionExistsError", err, err)
	}
}

func TestSessionCreateEmptyLaunchHandleFailsClosed(t *testing.T) {
	base := makeSessionBase(t)
	project := isolateSessionProject(t)
	writeProjectAmqrc(t, project)
	writeLaunchRoster(t, project, `{"schema":1,"agents":[{"handle":""},{"handle":"claude"}]}`)
	t.Chdir(project)

	_, err := captureEnvStdout(t, func() error {
		return runSessionCreate([]string{"--root", base, "squad"})
	})
	if err == nil {
		t.Fatal("empty launch.json handle succeeded")
	}
	if GetExitCode(err) != ExitError {
		t.Fatalf("exit code = %d, want %d: %v", GetExitCode(err), ExitError, err)
	}
	if _, statErr := os.Lstat(filepath.Join(base, "squad")); !os.IsNotExist(statErr) {
		t.Fatalf("empty-handle create wrote a session: %v", statErr)
	}
}

func TestSessionListJSONEmptyBase(t *testing.T) {
	base := t.TempDir()
	output, err := captureEnvStdout(t, func() error {
		return runSessionList([]string{"--root", base, "--json"})
	})
	if err != nil {
		t.Fatalf("list empty base: %v", err)
	}
	var result sessionListResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("empty list is not valid json: %v (%q)", err, output)
	}
	if result.Sessions == nil {
		t.Fatal("sessions is null, want empty array")
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("sessions = %#v, want empty", result.Sessions)
	}
}

func TestSessionListSymlinkIsNeverLegacy(t *testing.T) {
	base := makeSessionBase(t)
	realSession := filepath.Join(t.TempDir(), "Collab")
	if err := fsq.EnsureRootDirs(realSession); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(realSession, "claude"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	link := filepath.Join(base, "Collab")
	if err := os.Symlink(realSession, link); err != nil {
		t.Fatalf("symlink: %v", err)
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
	for _, item := range result.Sessions {
		if item.Name == "Collab" {
			t.Fatalf("symlinked session listed as %s: %#v", item.Kind, item)
		}
	}
	found := false
	for _, skipped := range result.Skipped {
		if skipped.Name == "Collab" && skipped.Reason == "symlink" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected Collab skipped as symlink, got %#v", result.Skipped)
	}

	text, err := captureEnvStdout(t, func() error {
		return runSessionList([]string{"--root", base})
	})
	if err != nil {
		t.Fatalf("text list: %v", err)
	}
	if strings.Contains(text, "Collab") {
		t.Fatalf("text list leaked symlink: %s", text)
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

func TestSessionResumeUnknownNameIsNotFoundWithZeroWrites(t *testing.T) {
	project := isolateSessionProject(t)
	for _, key := range []string{envRoot, envBaseRoot, envRootID, envBaseRootID, envSession, envGlobalRoot} {
		_ = os.Unsetenv(key)
	}
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	writeProjectAmqrc(t, project)
	writeLaunchRoster(t, project, `{"schema":1,"agents":[{"handle":"claude","command":["claude"]}]}`)
	base := filepath.Join(project, defaultCoopRoot)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	before := dirNames(t, base)
	err = runSession([]string{"resume", "missing"})
	if GetExitCode(err) != ExitNotFound {
		t.Fatalf("exit code = %d, want %d: %v", GetExitCode(err), ExitNotFound, err)
	}
	if after := dirNames(t, base); !slices.Equal(after, before) {
		t.Fatalf("unknown resume wrote state: before=%v after=%v", before, after)
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

func dirNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestPeelSessionResumeNameRejectsFlagAsName(t *testing.T) {
	_, _, err := peelSessionResumeName([]string{"--json"})
	if err == nil || GetExitCode(err) != ExitUsage {
		t.Fatalf("peelSessionResumeName(--json) = %v, want usage", err)
	}
	if !strings.Contains(err.Error(), "session name required") {
		t.Fatalf("error = %v, want session name required (not validateSessionName of --json)", err)
	}
}

func TestSessionListSkipsRegularFileAgentsDir(t *testing.T) {
	base := makeSessionBase(t)
	sess := filepath.Join(base, "collab")
	if err := os.Mkdir(sess, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "agents"), []byte("not a directory"), 0o600); err != nil {
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
	for _, item := range result.Sessions {
		if item.Name == "collab" {
			t.Fatalf("regular-file agents dir listed as session: %#v", item)
		}
	}
	found := false
	for _, skipped := range result.Skipped {
		if skipped.Name == "collab" && skipped.Reason == "not_a_session" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected collab skipped as not_a_session, got %#v", result.Skipped)
	}
}
