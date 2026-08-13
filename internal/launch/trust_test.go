package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func trustFixture(t *testing.T) (*TrustStore, string) {
	t.Helper()
	base := t.TempDir()
	project := filepath.Join(base, "project")
	state := filepath.Join(base, "state")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenTrustStore(state, project)
	if err != nil {
		t.Fatal(err)
	}
	return store, project
}

func testDigest(ch byte) string { return "sha256:" + strings.Repeat(string(ch), 64) }

func TestTrustRecordRoundTripAndSemanticInvalidation(t *testing.T) {
	store, _ := trustFixture(t)
	record := TrustRecord{
		SemanticDigest:    testDigest('a'),
		BypassArgs:        map[string][]string{"claude": {"--dangerously-skip-permissions"}},
		ArbitraryCommands: []ArbitraryCommandGrant{{Name: "operator-tool", Argv: []string{"/opt/tool", "run"}, Cwd: "/tmp"}},
	}
	if err := store.Replace(record); err != nil {
		t.Fatal(err)
	}
	got, trusted, err := store.LoadForDigest(record.SemanticDigest)
	if err != nil || !trusted {
		t.Fatalf("LoadForDigest = trusted %t, err %v", trusted, err)
	}
	if got.ProjectIdentity == "" || len(got.BypassArgs["claude"]) != 1 || len(got.ArbitraryCommands) != 1 {
		t.Fatalf("stored authority = %#v", got)
	}
	if info, err := os.Stat(store.Path()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trust record mode: info=%v err=%v", info, err)
	}

	stale, trusted, err := store.LoadForDigest(testDigest('b'))
	if err != nil || trusted {
		t.Fatalf("changed digest = trusted %t, err %v", trusted, err)
	}
	if len(stale.BypassArgs) != 0 || len(stale.ArbitraryCommands) != 0 {
		t.Fatalf("invalidated authority leaked: %#v", stale)
	}
}

func TestTrustStoreUsesPhysicalProjectIdentityAndDoesNotLeak(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	projectA := filepath.Join(base, "a")
	projectB := filepath.Join(base, "b")
	if err := os.Mkdir(projectA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(projectB, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasA := filepath.Join(base, "alias-a")
	if err := os.Symlink(projectA, aliasA); err != nil {
		t.Fatal(err)
	}
	storeA, err := OpenTrustStore(state, projectA)
	if err != nil {
		t.Fatal(err)
	}
	storeAlias, err := OpenTrustStore(state, aliasA)
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := OpenTrustStore(state, projectB)
	if err != nil {
		t.Fatal(err)
	}
	if storeA.Path() != storeAlias.Path() {
		t.Fatalf("physical alias got another trust key: %s != %s", storeA.Path(), storeAlias.Path())
	}
	if storeA.Path() == storeB.Path() {
		t.Fatal("different projects shared a trust key")
	}
	if err := storeA.Replace(TrustRecord{SemanticDigest: testDigest('c')}); err != nil {
		t.Fatal(err)
	}
	if _, trusted, err := storeB.LoadForDigest(testDigest('c')); err != nil || trusted {
		t.Fatalf("project B inherited project A trust: trusted=%t err=%v", trusted, err)
	}
}

func TestTrustLoadFailsClosed(t *testing.T) {
	store, _ := trustFixture(t)
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, data, want string }{
		{"malformed", `{`, "decode trust record"},
		{"downgrade", `{"version":0}`, "unsupported trust record version 0"},
		{"unknown field", `{"version":1,"project_identity":"x","semantic_digest":"` + testDigest('d') + `","extra":true}`, "unknown field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(store.Path(), []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, trusted, err := store.LoadForDigest(testDigest('d'))
			if trusted || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadForDigest = trusted %t, err %v, want %q", trusted, err, test.want)
			}
		})
	}

	valid := TrustRecord{Version: TrustVersion, ProjectIdentity: "another-project", SemanticDigest: testDigest('e')}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, trusted, err := store.LoadForDigest(valid.SemanticDigest); trusted || err == nil || !strings.Contains(err.Error(), "different project") {
		t.Fatalf("cross-project record = trusted %t, err %v", trusted, err)
	}

	if err := os.Chmod(store.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, trusted, err := store.LoadForDigest(valid.SemanticDigest); trusted || err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("permissive record = trusted %t, err %v", trusted, err)
	}
}

func TestTrustStoreRejectsInWorktreeState(t *testing.T) {
	project := t.TempDir()
	_, err := OpenTrustStore(filepath.Join(project, ".amq", "state"), project)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("OpenTrustStore error = %v, want outside-worktree refusal", err)
	}
	alias := filepath.Join(t.TempDir(), "state-alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	_, err = OpenTrustStore(filepath.Join(alias, ".amq", "state"), project)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("aliased OpenTrustStore error = %v, want outside-worktree refusal", err)
	}
}

func TestTrustLoadRejectsSymlinkRecord(t *testing.T) {
	store, _ := trustFixture(t)
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "trust.json")
	data := `{"version":1,"project_identity":"x","semantic_digest":"` + testDigest('f') + `"}`
	if err := os.WriteFile(outside, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, store.Path()); err != nil {
		t.Fatal(err)
	}
	if _, trusted, err := store.LoadForDigest(testDigest('f')); trusted || err == nil {
		t.Fatalf("symlink record = trusted %t, err %v", trusted, err)
	}
}

func TestTrustLoadRejectsInvalidRequestedDigest(t *testing.T) {
	store, _ := trustFixture(t)
	if _, trusted, err := store.LoadForDigest("not-a-digest"); trusted || err == nil {
		t.Fatalf("invalid requested digest = trusted %t, err %v", trusted, err)
	}
}
