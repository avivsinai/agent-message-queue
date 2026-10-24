package source_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
)

var githubNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func githubEnv(r fakeRunner) board.Env {
	return board.Env{
		Dir:      "/fixture",
		Now:      func() time.Time { return githubNow },
		Run:      r,
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		IsDir:    func(string) bool { return true },
	}
}

func githubOutputs() map[string]string {
	return map[string]string{
		"git remote get-url origin":        "origin-url.txt",
		"gh config get user -h github.com": "gh-user.txt",
		"gh api graphql":                   "github-graphql.json",
		"bd --readonly --sandbox list --json --brief --status open,in_progress,blocked,deferred --limit 0": "bd-list-github.json",
		// Branch names such as feat/pr-11 are candidate bead ids until bd
		// says they are not.
		"bd --readonly --sandbox list --all --id pr-11,pr-12,pr-13,pr-14 --json --brief --limit 0": "bd-lookup-empty.json",
	}
}

func demoGitHub() source.GitHub {
	return source.GitHub{
		Identities:               map[string]string{"octo-agent": "alpha"},
		ReadyToMergeWaitsOnHuman: true,
		TTL:                      10 * time.Minute,
	}
}

func cardText(c board.Card) string {
	parts := []string{c.Title, c.Reason.Text}
	parts = append(parts, c.Marks...)
	for _, it := range c.Items {
		parts = append(parts, it.Reason.Text)
		for _, e := range it.Evidence {
			parts = append(parts, e.Value, e.Text)
		}
	}
	return strings.Join(parts, " | ")
}

func TestGitHubPlacesCapturedGraphQLResponse(t *testing.T) {
	r := fakeRunner{t: t, outputs: githubOutputs()}
	sources := []board.Source{source.Beads{GitHubRepo: "octo-org/demo"}, demoGitHub()}
	m := board.Run(context.Background(), githubEnv(r), sources, board.DefaultConfig(), "test")
	cards := cardsByRef(m)
	pr := func(n string) board.Card { return cards["forge:github.com/octo-org/demo#"+n] }

	if c := pr("11"); c.Column != board.ColumnWaiting || c.Reason.Text != "ready to merge" {
		t.Errorf("#11 = %s %q, want ready to merge in Waiting on you", c.Column, c.Reason.Text)
	}
	if c := pr("14"); c.Column != board.ColumnWaiting || c.Reason.Text != "your review requested" {
		t.Errorf("#14 = %s %q", c.Column, c.Reason.Text)
	}
	// A review request found only by the search still reaches the queue.
	if c := pr("40"); c.Column != board.ColumnWaiting {
		t.Errorf("#40 = %s, want Waiting on you", c.Column)
	}
	if c := pr("16"); c.Column != board.ColumnInFlight || len(c.WaitsOn) != 1 || c.WaitsOn[0].Handle != "alpha" {
		t.Errorf("#16 = %s waits on %+v, want the author", c.Column, c.WaitsOn)
	}
	// A branch named fix/demo-g1-clock names bead demo-g1 (a hyphen prefix
	// of the token), as a related reference only.
	if !hasToken(pr("16"), "bead:demo-g1", "branch name") {
		t.Errorf("#16 related = %+v", pr("16").Related)
	}
	if c := cards["forge:github.com/octo-org/demo#20"]; c.Column != board.ColumnUnclaimed {
		t.Errorf("issue #20 = %s", c.Column)
	}

	// Pro finding 10: approved with passing checks is not "ready to merge"
	// while the PR is a draft or conflicting; the facts are evidence.
	for n, fact := range map[string]string{"12": "draft", "13": "conflicting"} {
		c := pr(n)
		if c.Column != board.ColumnInFlight {
			t.Errorf("#%s = %s, want In flight", n, c.Column)
		}
		text := cardText(c)
		if strings.Contains(text, "ready to merge") || !strings.Contains(text, fact) || !strings.Contains(text, "approved; checks passing") {
			t.Errorf("#%s card text = %q", n, text)
		}
	}

	// Pro finding 9: the bead's external_ref and the PR's own ref are one
	// ForgeItem, so they are one card with both items.
	merged, ok := cards["bead:demo-g1"]
	if !ok {
		merged = pr("15")
	}
	if len(merged.Items) != 2 {
		t.Fatalf("demo-g1 and #15: %d items on the card, want 2", len(merged.Items))
	}

	// Pro finding 8: a page with more PRs is partial, not complete.
	gh := source_(m, "github")
	if gh.Coverage.Truncated != 20 || !gh.Partial || !strings.Contains(gh.Coverage.Detail, "20 more PRs not fetched") {
		t.Errorf("github coverage = %+v", gh.Coverage)
	}
}

func TestGitHubCachedReadMakesOneCall(t *testing.T) {
	cache := source.DirCache{Dir: t.TempDir()}
	calls := map[string]int{}
	r := fakeRunner{t: t, outputs: githubOutputs(), calls: calls}
	g := demoGitHub()
	g.Cache = cache

	// Two runs within the TTL make one GraphQL call; --refresh makes one more.
	for range 2 {
		if res := g.Read(context.Background(), githubEnv(r)); res.Status != board.StatusOK {
			t.Fatalf("read: %s %s", res.Status, res.Detail)
		}
	}
	g.Refresh = true
	g.Read(context.Background(), githubEnv(r))
	if calls["gh api graphql"] != 2 {
		t.Errorf("graphql calls = %d, want 2", calls["gh api graphql"])
	}

}

// Merge review of the board core and GitHub source: a gh response that
// did not answer the query read as an ok, empty GitHub source.
func TestGitHubUnansweredQueryIsDegraded(t *testing.T) {
	for _, body := range []string{
		`{"data":null}`,
		`{"message":"Bad credentials"}`,
		`{"data":{"viewer":{"login":"me"}}}`,
	} {
		res := source.GitHub{}.Build([]byte(body), []string{"octo-org/demo"}, githubNow, nil)
		if res.Status != board.StatusDegraded {
			t.Errorf("%s: status %s %q, want degraded", body, res.Status, res.Detail)
		}
	}
}

func TestExternalProviderIsDataOnly(t *testing.T) {
	ext := source.External{Provider: board.Provider{Name: "tracker", Exec: "/opt/tracker/board", Enabled: true}}
	r := fakeRunner{t: t, outputs: map[string]string{"/opt/tracker/board": "provider.json"}}
	m := board.Run(context.Background(), githubEnv(r), []board.Source{ext}, board.DefaultConfig(), "test")

	src := source_(m, "ext:tracker")
	if src.Status != board.StatusOK || src.Coverage.Skipped != 2 {
		t.Fatalf("ext:tracker = %s, skipped %d", src.Status, src.Coverage.Skipped)
	}
	cards := cardsByRef(m)
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1 (hostile items dropped)", len(cards))
	}
	for _, c := range cards {
		// The document claims source "amq"; the core assigns ext:<name>.
		if c.Items[0].Source != "ext:tracker" || c.ID != "ext:tracker:pr:demo/repo#2411" {
			t.Errorf("card %s from %s", c.ID, c.Items[0].Source)
		}
		if len(c.Actions) != 0 {
			t.Errorf("provider item has actions: %+v", c.Actions)
		}
	}
}

// Pro finding 7: a project file cannot set a provider's executable.
func TestProjectConfigCannotSetProviderExec(t *testing.T) {
	dir := t.TempDir()
	user := writeFile(t, dir, "user.json", `{"providers":[{"name":"tracker","exec":"/opt/tracker/board"}]}`)
	project := writeFile(t, dir, "project.json", `{"providers":[{"name":"tracker","exec":"/bin/sh","args":["-c","touch x"]},{"name":"other","enabled":true}],"stale_after":"2d"}`)
	cfg, warnings := board.LoadConfig(user, project)
	if len(cfg.Providers) != 1 || cfg.Providers[0].Exec != "/opt/tracker/board" || len(cfg.Providers[0].Args) != 0 {
		t.Errorf("providers = %+v", cfg.Providers)
	}
	if cfg.Board.StaleAfter != 48*time.Hour {
		t.Errorf("stale_after = %s", cfg.Board.StaleAfter)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, `provider "tracker": exec, args, and ttl may be set only in the user file`) ||
		!strings.Contains(joined, `provider "other" is not registered`) {
		t.Errorf("warnings = %q", joined)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
