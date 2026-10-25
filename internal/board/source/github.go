package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// GitHubQuerySchema versions the query and its cache entries: a cache entry
// written for another schema is a miss.
const GitHubQuerySchema = "amq/board-github/v2"

// maxGitHubRepos bounds the aliases in the one query.
const maxGitHubRepos = 10

// maxBodyBytes is how much of a PR body is kept for bead id mentions;
// GraphQL cannot truncate on the server.
const maxBodyBytes = 4096

// GitHub reads open pull requests and issues of the project's GitHub
// repositories with one `gh api graphql` call, cached on disk for the TTL,
// because each gh call that reads the token can prompt the keychain.
type GitHub struct {
	Repos                    []string          // configured owner/repo, added to the origin remote
	Humans                   []string          // GitHub logins of the owner
	Identities               map[string]string // login -> AMQ handle
	ReadyToMergeWaitsOnHuman bool
	TTL                      time.Duration
	Refresh                  bool  // ignore a fresh cache entry
	Cache                    Cache // nil: no cache
	// Account is the viewer login of a token the caller holds and passes
	// to gh (serve's token_once). It replaces the configured login as the
	// cache identity, since an account switch changes that login but not
	// the token. Empty: the configured login.
	Account string
}

func (GitHub) Name() string { return "github" }

func (GitHub) Discover(env board.Env) (bool, string) {
	if _, err := env.LookPath("gh"); err != nil {
		return false, "gh not on PATH"
	}
	return true, ""
}

var githubFeeds = []board.Column{board.ColumnWaiting, board.ColumnInFlight, board.ColumnUnclaimed}

// GitHubRepoSet is the repo set a read uses: the origin remote when it is on
// github.com, plus the configured repos, normalized and sorted.
func (g GitHub) RepoSet(ctx context.Context, env board.Env) []string {
	repos := append([]string(nil), g.Repos...)
	if out, err := env.Run.Run(ctx, env.Dir, []string{"git", "remote", "get-url", "origin"}); err == nil {
		if r, ok := GitHubRemoteRepo(strings.TrimSpace(string(out))); ok {
			repos = append(repos, r)
		}
	}
	return board.NormalizeRepos(repos)
}

func (g GitHub) Read(ctx context.Context, env board.Env) board.Result {
	repos := g.RepoSet(ctx, env)
	if len(repos) == 0 {
		return board.Result{Status: board.StatusUnavailable, Detail: "no github.com origin remote and no forges.github.repos"}
	}
	if len(repos) > maxGitHubRepos {
		res := board.Result{Status: board.StatusDegraded, Detail: fmt.Sprintf("%d repos configured; at most %d", len(repos), maxGitHubRepos)}
		return res
	}
	key := CacheKey{Host: "github.com", Repos: repos, Schema: GitHubQuerySchema}
	if g.Cache == nil {
		res, _ := g.fetch(ctx, env, repos)
		return res
	}
	// The configured login, known before any gh call that reads the token,
	// names the refresh lock and this read's records. The response itself is
	// stored only under the viewer login it names, and a viewer record maps
	// the configured login to that viewer, so a renamed account still finds
	// its entry and one viewer's data never sits under another login.
	login := key
	if login.Account = g.Account; login.Account == "" {
		login.Account = githubAccount(ctx, env)
	}
	if login.Account == "" {
		return g.refresh(ctx, env, repos, key, false)
	}
	if res, ok := g.cached(login, repos, env.Now()); ok {
		return res
	}
	// Each gh call can prompt the keychain, so board runs refresh one at a
	// time: a run that waited reads the entry the other run wrote, or the
	// failure it recorded, instead of calling gh again.
	var res board.Result
	err := g.Cache.Locked(login, func() bool { return ctx.Err() != nil }, func() error {
		var ok bool
		if res, ok = g.cached(login, repos, env.Now()); ok {
			return nil
		}
		if res, ok = g.failedRecently(login, repos, env.Now()); ok {
			return nil
		}
		res = g.refresh(ctx, env, repos, login, true)
		return nil
	})
	switch {
	case err == nil:
		return res
	case errors.Is(err, lock.ErrStopped):
		return g.lastAs(login, repos, env.Now(), "another board run is refreshing GitHub")
	default:
		// No lock to take (a platform without advisory locks, an unusable
		// cache directory): refresh unlocked, as a read without a cache does.
		return g.refresh(ctx, env, repos, login, true)
	}
}

// refreshFailureHold is how long a failed refresh stands for the next runs,
// which show the last entry with the failure instead of calling gh again.
const refreshFailureHold = 60 * time.Second

const (
	githubViewerSchema = GitHubQuerySchema + "/viewer"
	githubFailedSchema = GitHubQuerySchema + "/refresh-failed"
)

type githubViewerRecord struct {
	Viewer string `json:"viewer"`
}

type githubFailedRecord struct {
	Detail string `json:"detail"`
}

func (g GitHub) putRecord(login CacheKey, schema string, at time.Time, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	login.Schema = schema
	return g.Cache.Put(login, at, data)
}

func (g GitHub) record(login CacheKey, schema string, v any) (time.Time, bool) {
	login.Schema = schema
	data, at, ok := g.Cache.Last(login)
	if !ok || json.Unmarshal(data, v) != nil {
		return time.Time{}, false
	}
	return at, true
}

// entryKey is the key of the entry the last successful refresh under login
// stored: the same repos and query, under the viewer login.
func (g GitHub) entryKey(login CacheKey) (CacheKey, bool) {
	var v githubViewerRecord
	if _, ok := g.record(login, githubViewerSchema, &v); !ok || v.Viewer == "" {
		return CacheKey{}, false
	}
	login.Account = v.Viewer
	return login, true
}

// cached is the read a fresh entry for login serves, unless Refresh is set.
// A recent failed refresh newer than the entry degrades it.
func (g GitHub) cached(login CacheKey, repos []string, now time.Time) (board.Result, bool) {
	if g.Refresh {
		return board.Result{}, false
	}
	key, ok := g.entryKey(login)
	if !ok {
		return board.Result{}, false
	}
	resp, fetchedAt, ok := g.Cache.Get(key, now, g.TTL)
	if !ok {
		return board.Result{}, false
	}
	if res, failed := g.failedRecently(login, repos, now); failed {
		return res, true
	}
	return g.Build(resp, repos, fetchedAt, nil), true
}

// failedRecently is the read while a refresh under login that failed after
// the last stored entry is younger than refreshFailureHold. Refresh skips it.
func (g GitHub) failedRecently(login CacheKey, repos []string, now time.Time) (board.Result, bool) {
	if g.Refresh {
		return board.Result{}, false
	}
	var f githubFailedRecord
	failedAt, ok := g.record(login, githubFailedSchema, &f)
	if age := now.Sub(failedAt); !ok || age < 0 || age >= refreshFailureHold {
		return board.Result{}, false
	}
	if key, ok := g.entryKey(login); ok {
		if _, fetchedAt, ok := g.Cache.Last(key); ok && !fetchedAt.Before(failedAt) {
			return board.Result{}, false
		}
	}
	return g.lastAs(login, repos, now, "GitHub refresh failed at "+failedAt.UTC().Format(time.RFC3339)+": "+f.Detail), true
}

// lastAs is the last entry for login at any age, degraded with detail, or a
// degraded result without items when there is none. It never calls gh.
func (g GitHub) lastAs(login CacheKey, repos []string, now time.Time, detail string) board.Result {
	if key, ok := g.entryKey(login); ok {
		if resp, fetchedAt, ok := g.Cache.Last(key); ok {
			res := g.Build(resp, repos, fetchedAt, nil)
			if res.Status == board.StatusOK {
				res.Status, res.Detail = board.StatusDegraded, detail+"; showing data from "+fetchedAt.UTC().Format(time.RFC3339)
				return res
			}
		}
	}
	return board.Result{Status: board.StatusDegraded, Detail: detail + "; no cached data", ObservedAt: now, Feeds: githubFeeds, Version: GitHubQuerySchema}
}

// refresh calls gh and stores an OK response under the viewer login it
// names. With record set, login is the configured login: a success records
// login's viewer, and a failure is recorded for refreshFailureHold and shows
// the last entry when the failed read has no items of its own.
func (g GitHub) refresh(ctx context.Context, env board.Env, repos []string, login CacheKey, record bool) board.Result {
	now := env.Now()
	res, out := g.fetch(ctx, env, repos)
	if res.Status != board.StatusOK {
		if !record {
			return res
		}
		// Stamped when the call returned, so a slow failing call does not
		// use up the hold.
		failedAt := env.Now()
		recErr := g.putRecord(login, githubFailedSchema, failedAt, githubFailedRecord{Detail: res.Detail})
		if len(res.Items) == 0 {
			if last := g.lastAs(login, repos, failedAt, "GitHub refresh failed: "+res.Detail); len(last.Items) > 0 {
				res = last
			}
		}
		if recErr != nil {
			res.Detail += "; failure not recorded: " + recErr.Error()
		}
		return res
	}
	viewer := githubViewer(out)
	if viewer == "" {
		res.Detail = "cache not written: response names no viewer"
		return res
	}
	key := login
	key.Account = viewer
	err := g.Cache.Put(key, now, out)
	if err == nil && record {
		err = g.putRecord(login, githubViewerSchema, now, githubViewerRecord{Viewer: viewer})
	}
	if err != nil {
		res.Detail = "cache not written: " + err.Error()
	}
	return res
}

// fetch runs the query and its review-request pages.
func (g GitHub) fetch(ctx context.Context, env board.Env, repos []string) (board.Result, []byte) {
	now := env.Now()
	out, err := env.Run.Run(ctx, env.Dir, GitHubQueryArgv(repos))
	if err != nil {
		return g.Build(nil, repos, now, err), nil
	}
	out, pageErr := githubAttentionPages(ctx, env, repos, out)
	res := g.Build(out, repos, now, nil)
	if pageErr != nil && res.Status == board.StatusOK {
		res.Status, res.Detail = board.StatusDegraded, "review request search: "+errLine(pageErr)
	}
	return res, out
}

// githubAccount is the account a gh call runs as, or "" when it is not
// known. GH_TOKEN and GITHUB_TOKEN take precedence over the stored login, so
// with either set the configured user says nothing about whose data a cached
// entry holds.
func githubAccount(ctx context.Context, env board.Env) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if os.Getenv(name) != "" {
			return ""
		}
	}
	out, err := env.Run.Run(ctx, env.Dir, []string{"gh", "config", "get", "user", "-h", "github.com"})
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(out)))
}

var githubRemoteRe = regexp.MustCompile(`^(?:git@github\.com:|ssh://git@github\.com/|https://github\.com/)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// GitHubRemoteRepo extracts owner/repo from a github.com remote URL.
func GitHubRemoteRepo(url string) (string, bool) {
	m := githubRemoteRe.FindStringSubmatch(url)
	if m == nil {
		return "", false
	}
	return m[1] + "/" + m[2], true
}

const githubPRFields = `number title body url headRefName isDraft mergeable reviewDecision createdAt updatedAt
        author { login }
        reviewRequests(first: 10) { totalCount pageInfo { hasNextPage }
          nodes { requestedReviewer { ... on User { login } ... on Team { slug } } } }
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }`

// GitHubQueryArgv builds the one query: one alias per repo plus the search
// for PRs that request the viewer's review. Owner and repo names reach the
// query only as variables.
func GitHubQueryArgv(repos []string) []string {
	var vars, aliases strings.Builder
	argv := []string{"gh", "api", "graphql", "--hostname", "github.com"}
	for i, r := range repos {
		owner, name, _ := strings.Cut(r, "/")
		fmt.Fprintf(&vars, ", $owner%d: String!, $name%d: String!", i, i)
		fmt.Fprintf(&aliases, `
  r%d: repository(owner: $owner%d, name: $name%d) {
    pullRequests(states: OPEN, first: 50, orderBy: {field: UPDATED_AT, direction: DESC}) {
      totalCount
      pageInfo { hasNextPage }
      nodes { %s }
    }
    issues(states: OPEN, first: 50, orderBy: {field: UPDATED_AT, direction: DESC}) {
      totalCount
      pageInfo { hasNextPage }
      nodes { number title url updatedAt assignees(first: 5) { nodes { login } } }
    }
  }`, i, i, i, githubPRFields)
		argv = append(argv, "-f", fmt.Sprintf("owner%d=%s", i, owner), "-f", fmt.Sprintf("name%d=%s", i, name))
	}
	query := fmt.Sprintf(`query Board($attention: String!%s) {
  viewer { login }%s
  attention: search(type: ISSUE, first: 50, query: $attention) {%s
  }
}`, vars.String(), aliases.String(), githubAttentionFields)
	return append(argv, "-f", "attention="+githubAttentionQuery(repos), "-f", "query="+query)
}

const githubAttentionFields = `
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes { ... on PullRequest { number title url createdAt updatedAt author { login } repository { nameWithOwner } } }`

// maxAttentionPages bounds the review-request search per read, the first
// page included; what is left over is counted as not fetched.
const maxAttentionPages = 5

func githubAttentionQuery(repos []string) string {
	q := "is:pr is:open review-requested:@me"
	for _, r := range repos {
		q += " repo:" + r
	}
	return q
}

func githubAttentionPageArgv(repos []string, after string) []string {
	query := `query BoardAttention($attention: String!, $after: String!) {
  attention: search(type: ISSUE, first: 50, after: $after, query: $attention) {` + githubAttentionFields + `
  }
}`
	return []string{"gh", "api", "graphql", "--hostname", "github.com",
		"-f", "attention=" + githubAttentionQuery(repos), "-f", "after=" + after, "-f", "query=" + query}
}

type ghAttention struct {
	IssueCount int `json:"issueCount"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []json.RawMessage `json:"nodes"`
}

// githubAttentionPages follows the review-request search of the first
// response for up to maxAttentionPages pages and returns that response with
// the nodes of every fetched page and the last page's pageInfo, so the
// result and its cache entry stay one document. A response it cannot read is
// returned unchanged for Build to judge. A failed page stops the paging; the
// pages fetched so far are kept and the error is returned.
func githubAttentionPages(ctx context.Context, env board.Env, repos []string, resp []byte) ([]byte, error) {
	var top, data map[string]json.RawMessage
	if json.Unmarshal(resp, &top) != nil || top["errors"] != nil || json.Unmarshal(top["data"], &data) != nil || data == nil {
		return resp, nil
	}
	var att ghAttention
	if raw, ok := data["attention"]; !ok || json.Unmarshal(raw, &att) != nil {
		return resp, nil
	}
	if !att.PageInfo.HasNextPage {
		return resp, nil
	}
	var pageErr error
	for page := 1; page < maxAttentionPages && att.PageInfo.HasNextPage && att.PageInfo.EndCursor != ""; page++ {
		out, err := env.Run.Run(ctx, env.Dir, githubAttentionPageArgv(repos, att.PageInfo.EndCursor))
		if err != nil {
			pageErr = err
			break
		}
		var next struct {
			Data struct {
				Attention *ghAttention `json:"attention"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(out, &next); err != nil {
			pageErr = fmt.Errorf("page %d unreadable: %w", page+1, err)
			break
		}
		if len(next.Errors) > 0 || next.Data.Attention == nil {
			msg := "no answer"
			if len(next.Errors) > 0 {
				msg = next.Errors[0].Message
			}
			pageErr = fmt.Errorf("page %d: %s", page+1, msg)
			break
		}
		att.Nodes = append(att.Nodes, next.Data.Attention.Nodes...)
		att.PageInfo = next.Data.Attention.PageInfo
		att.IssueCount = next.Data.Attention.IssueCount
	}
	var err error
	if data["attention"], err = json.Marshal(att); err != nil {
		return resp, err
	}
	if top["data"], err = json.Marshal(data); err != nil {
		return resp, err
	}
	merged, err := json.Marshal(top)
	if err != nil {
		return resp, err
	}
	return merged, pageErr
}

type ghLogin struct {
	Login string `json:"login"`
}

type ghPR struct {
	Number         int      `json:"number"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	URL            string   `json:"url"`
	HeadRefName    string   `json:"headRefName"`
	IsDraft        bool     `json:"isDraft"`
	Mergeable      string   `json:"mergeable"`
	ReviewDecision *string  `json:"reviewDecision"`
	CreatedAt      string   `json:"createdAt"`
	UpdatedAt      string   `json:"updatedAt"`
	Author         *ghLogin `json:"author"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer struct {
				Login string `json:"login"`
				Slug  string `json:"slug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type ghIssue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	UpdatedAt string `json:"updatedAt"`
	Assignees struct {
		Nodes []ghLogin `json:"nodes"`
	} `json:"assignees"`
}

type ghPage struct {
	TotalCount int `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pageInfo"`
}

type ghRepo struct {
	PullRequests struct {
		ghPage
		Nodes []ghPR `json:"nodes"`
	} `json:"pullRequests"`
	Issues struct {
		ghPage
		Nodes []ghIssue `json:"nodes"`
	} `json:"issues"`
}

type ghResponse struct {
	Data struct {
		Viewer    ghLogin `json:"viewer"`
		Attention struct {
			IssueCount int `json:"issueCount"`
			PageInfo   struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pageInfo"`
			Nodes []struct {
				Number     int      `json:"number"`
				Title      string   `json:"title"`
				URL        string   `json:"url"`
				CreatedAt  string   `json:"createdAt"`
				UpdatedAt  string   `json:"updatedAt"`
				Author     *ghLogin `json:"author"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"nodes"`
		} `json:"attention"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func githubViewer(resp []byte) string {
	var r ghResponse
	if json.Unmarshal(resp, &r) != nil {
		return ""
	}
	return strings.ToLower(r.Data.Viewer.Login)
}

// Build maps one GraphQL response for the repo set (alias r<i> is repos[i])
// into a result. It is pure.
func (g GitHub) Build(resp []byte, repos []string, fetchedAt time.Time, runErr error) board.Result {
	res := board.Result{Status: board.StatusOK, ObservedAt: fetchedAt, Feeds: githubFeeds, Version: GitHubQuerySchema}
	if runErr != nil {
		res.Status, res.Detail = board.StatusDegraded, "gh api graphql failed: "+errLine(runErr)
		return res
	}
	var r ghResponse
	if err := json.Unmarshal(resp, &r); err != nil {
		res.Status, res.Detail = board.StatusDegraded, "GraphQL response unreadable: "+err.Error()
		return res
	}
	if len(r.Errors) > 0 {
		res.Status, res.Detail = board.StatusDegraded, "GraphQL error: "+r.Errors[0].Message
		return res
	}
	results, err := githubRepos(resp, repos)
	if err != nil {
		res.Status, res.Detail = board.StatusDegraded, "GraphQL response unreadable: "+err.Error()
		return res
	}
	m := githubMapper{g: g, viewer: strings.ToLower(r.Data.Viewer.Login), attention: map[board.Ref]bool{}}
	for _, n := range r.Data.Attention.Nodes {
		if owner, name, ok := strings.Cut(n.Repository.NameWithOwner, "/"); ok {
			if ref, ok := board.ForgeItem("github.com", owner, name, n.Number); ok {
				m.attention[ref] = true
			}
		}
	}

	var gaps []string
	seen := map[board.Ref]bool{}
	morePRs, moreIssues := 0, 0
	for _, rr := range results {
		for _, pr := range rr.repo.PullRequests.Nodes {
			if it, ok := m.pr(rr.slug, pr); ok {
				seen[it.Ref] = true
				res.Items = append(res.Items, it)
			} else {
				res.Coverage.Skipped++
			}
		}
		for _, is := range rr.repo.Issues.Nodes {
			if it, ok := m.issue(rr.slug, is); ok {
				res.Items = append(res.Items, it)
			} else {
				res.Coverage.Skipped++
			}
		}
		if rr.repo.PullRequests.PageInfo.HasNextPage {
			morePRs += rr.repo.PullRequests.TotalCount - len(rr.repo.PullRequests.Nodes)
		}
		if rr.repo.Issues.PageInfo.HasNextPage {
			moreIssues += rr.repo.Issues.TotalCount - len(rr.repo.Issues.Nodes)
		}
	}
	// A review requested on a PR past the first page still reaches the
	// owner's queue from the search.
	for _, n := range r.Data.Attention.Nodes {
		owner, name, _ := strings.Cut(n.Repository.NameWithOwner, "/")
		ref, ok := board.ForgeItem("github.com", owner, name, n.Number)
		if !ok || seen[ref] {
			continue
		}
		pr := ghPR{Number: n.Number, Title: n.Title, URL: n.URL, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, Author: n.Author}
		if it, ok := m.pr(owner+"/"+name, pr); ok {
			res.Items = append(res.Items, it)
		}
	}
	moreAttention := 0
	if r.Data.Attention.PageInfo.HasNextPage {
		moreAttention = r.Data.Attention.IssueCount - len(r.Data.Attention.Nodes)
	}
	res.Coverage.Truncated = morePRs + moreIssues + moreAttention
	if morePRs > 0 {
		gaps = append(gaps, fmt.Sprintf("%d more PRs not fetched", morePRs))
	}
	if moreIssues > 0 {
		gaps = append(gaps, fmt.Sprintf("%d more issues not fetched", moreIssues))
	}
	if moreAttention > 0 {
		gaps = append(gaps, fmt.Sprintf("%d more review requests not fetched", moreAttention))
	}
	if res.Coverage.Skipped > 0 {
		gaps = append(gaps, fmt.Sprintf("%d records failed validation", res.Coverage.Skipped))
	}
	res.Coverage.Detail = strings.Join(gaps, "; ")
	return res
}

type repoResult struct {
	slug string
	repo ghRepo
}

// githubRepos reads alias r<i> as repos[i]. A null alias (a repo that does
// not exist or is not visible) yields nothing. A response without data, or
// without an alias the query asked for, did not answer the query: it is an
// error, never an empty repo set.
func githubRepos(resp []byte, repos []string) ([]repoResult, error) {
	var raw struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp, &raw); err != nil {
		return nil, err
	}
	if raw.Data == nil {
		return nil, errors.New("no data")
	}
	var out []repoResult
	for i, slug := range repos {
		msg, ok := raw.Data["r"+strconv.Itoa(i)]
		if !ok {
			return nil, fmt.Errorf("no answer for %s", slug)
		}
		if string(msg) == "null" {
			continue
		}
		var repo ghRepo
		if err := json.Unmarshal(msg, &repo); err != nil {
			return nil, err
		}
		out = append(out, repoResult{slug: slug, repo: repo})
	}
	return out, nil
}

var githubItemURLRe = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/(?:pull|issues)/[0-9]+$`)

type githubMapper struct {
	g         GitHub
	viewer    string
	attention map[board.Ref]bool
}

func (m githubMapper) owner(login string) board.Owner {
	l := strings.ToLower(login)
	switch l {
	case "":
		return board.Owner{Kind: board.OwnerUnmapped, Display: "ghost"}
	case m.viewer:
		return board.Owner{Kind: board.OwnerHuman, Display: login}
	}
	for _, h := range m.g.Humans {
		if strings.EqualFold(h, login) {
			return board.Owner{Kind: board.OwnerHuman, Display: login}
		}
	}
	if handle, ok := m.g.Identities[l]; ok {
		return board.Owner{Kind: board.OwnerAgent, Handle: handle, Display: login}
	}
	return board.Owner{Kind: board.OwnerUnmapped, Display: login}
}

func httpsGitHubURL(u string) string {
	if githubItemURLRe.MatchString(u) {
		return u
	}
	return ""
}

func (m githubMapper) pr(slug string, pr ghPR) (board.Item, bool) {
	owner, name, _ := strings.Cut(slug, "/")
	ref, ok := board.ForgeItem("github.com", owner, name, pr.Number)
	if !ok {
		return board.Item{}, false
	}
	updated, _ := time.Parse(time.RFC3339, pr.UpdatedAt)
	created, _ := time.Parse(time.RFC3339, pr.CreatedAt)
	author := ""
	if pr.Author != nil {
		author = pr.Author.Login
	}
	authorOwner := m.owner(author)
	url := httpsGitHubURL(pr.URL)
	decision := ""
	if pr.ReviewDecision != nil {
		decision = *pr.ReviewDecision
	}
	checks := ""
	if n := pr.Commits.Nodes; len(n) > 0 && n[len(n)-1].Commit.StatusCheckRollup != nil {
		checks = n[len(n)-1].Commit.StatusCheckRollup.State
	}
	it := board.Item{
		Ref:        ref,
		Title:      pr.Title,
		Owner:      authorOwner,
		WaitsOn:    []board.Owner{},
		Relations:  []board.Relation{},
		UpdatedAt:  updated,
		Structural: true,
		Evidence: []board.Evidence{
			{Field: "type", Value: board.ForgePullRequest, Text: "PR #" + strconv.Itoa(pr.Number), At: updated, URL: url},
		},
	}
	fact := func(field, value string) {
		if value != "" {
			it.Evidence = append(it.Evidence, board.Evidence{Field: field, Value: value, At: updated})
		}
	}
	fact("reviewDecision", decision)
	fact("mergeable", pr.Mergeable)
	fact("checks", checks)
	if pr.IsDraft {
		fact("isDraft", "true")
	}

	var reviewers []board.Owner
	viewerRequested := m.attention[ref]
	for _, n := range pr.ReviewRequests.Nodes {
		rv := n.RequestedReviewer
		switch {
		case rv.Login != "":
			if strings.EqualFold(rv.Login, m.viewer) {
				viewerRequested = true
			}
			reviewers = append(reviewers, m.owner(rv.Login))
		case rv.Slug != "":
			reviewers = append(reviewers, board.Owner{Kind: board.OwnerUnmapped, Display: "team " + rv.Slug})
		}
	}
	viewerOwner := board.Owner{Kind: board.OwnerHuman, Display: m.viewer}
	approvedAndGreen := decision == "APPROVED" && checks == "SUCCESS"
	switch {
	case viewerRequested:
		it.State = board.StateNeedsHuman
		it.WaitsOn = []board.Owner{viewerOwner}
		it.Reason = board.Reason{Field: "reviewRequests", Text: "your review requested"}
	case approvedAndGreen && !pr.IsDraft && pr.Mergeable == "MERGEABLE" && authorOwner.Kind == board.OwnerAgent && m.g.ReadyToMergeWaitsOnHuman:
		it.State = board.StateNeedsHuman
		it.WaitsOn = []board.Owner{viewerOwner}
		it.Reason = board.Reason{Field: "reviewDecision", Text: "ready to merge"}
	default:
		it.State = board.StateActive
		var facts []string
		switch {
		case decision == "CHANGES_REQUESTED":
			it.WaitsOn = []board.Owner{authorOwner}
			facts = append(facts, "changes requested")
		case checks == "FAILURE" || checks == "ERROR":
			it.WaitsOn = []board.Owner{authorOwner}
			facts = append(facts, "checks failing")
		case len(reviewers) > 0:
			it.WaitsOn = reviewers
			facts = append(facts, "review requested")
		}
		if approvedAndGreen {
			facts = append(facts, "approved", "checks passing")
		}
		if pr.IsDraft {
			facts = append(facts, "draft")
		}
		if pr.Mergeable == "CONFLICTING" {
			facts = append(facts, "conflicting")
		}
		// A plain open PR has been open since createdAt. Every fact above
		// (a review request, requested changes, checks, approval, draft,
		// conflict) can begin later at a time no fetched field records, so
		// its since stays unknown rather than too early.
		text, since := "open PR", created
		if len(facts) > 0 {
			text, since = strings.Join(facts, "; "), time.Time{}
		}
		it.Reason = board.Reason{Field: "reviewDecision", Text: text, Since: since}
	}

	if br, ok := board.Branch("github.com", owner, name, pr.HeadRefName); ok {
		it.Relations = append(it.Relations, board.Relation{Kind: board.RelationRelated, To: br, Basis: "headRefName"})
	}
	body := pr.Body
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes]
	}
	for _, src := range []struct{ text, basis string }{{pr.Title, "title mention"}, {body, "body mention"}, {pr.HeadRefName, "branch name"}} {
		for _, tok := range board.BeadTokens(src.text) {
			it.Mentions = append(it.Mentions, board.Mention{Token: tok, Basis: src.basis})
		}
	}
	return it, true
}

func (m githubMapper) issue(slug string, is ghIssue) (board.Item, bool) {
	owner, name, _ := strings.Cut(slug, "/")
	ref, ok := board.ForgeItem("github.com", owner, name, is.Number)
	if !ok {
		return board.Item{}, false
	}
	updated, _ := time.Parse(time.RFC3339, is.UpdatedAt)
	it := board.Item{
		Ref:        ref,
		Title:      is.Title,
		Owner:      board.Owner{Kind: board.OwnerNone},
		WaitsOn:    []board.Owner{},
		Relations:  []board.Relation{},
		UpdatedAt:  updated,
		Structural: true,
		Evidence:   []board.Evidence{{Field: "type", Value: board.ForgeIssue, Text: "issue #" + strconv.Itoa(is.Number), At: updated, URL: httpsGitHubURL(is.URL)}},
	}
	if a := is.Assignees.Nodes; len(a) > 0 {
		it.Owner = m.owner(a[0].Login)
		it.State = board.StateActive
		it.Reason = board.Reason{Field: "assignees", Text: "issue assigned"}
	} else {
		it.State = board.StateOpenUnowned
		it.Reason = board.Reason{Field: "assignees", Text: "issue open, no assignee"}
	}
	for _, tok := range board.BeadTokens(is.Title) {
		it.Mentions = append(it.Mentions, board.Mention{Token: tok, Basis: "title mention"})
	}
	return it, true
}
