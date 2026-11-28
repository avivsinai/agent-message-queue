package board

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// Design 9.1 and slice 6 (finding 15): source text reaches the page only
// escaped, the embedded page model parses back equal, the page script builds no
// markup from strings, and a hostile handle yields no command.
func TestHTMLEscapesHostileTextAndRoundTripsTheModel(t *testing.T) {
	const hostile = `"><img src=x onerror=alert(1)>';touch /tmp/x;'</script><!--` + "\u2028"
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	bead, _ := Bead("demo-a1b")
	obligation, _ := Obligation("s1/alpha/demo")
	agent := Owner{Kind: OwnerAgent, Handle: hostile, Session: hostile, Display: hostile}
	amq := Result{
		Status: StatusDegraded, Detail: hostile, ObservedAt: now,
		Coverage: Coverage{Window: hostile, Detail: hostile + " gap"},
		Feeds:    []Column{ColumnWaiting, ColumnBlocked},
		AMQ: &AMQContext{
			Project: hostile, Peers: []string{hostile},
			Sessions: []SessionInfo{{Name: hostile, Path: hostile}},
			Gates:    []Gate{{Session: hostile, OpenCount: 1}},
		},
		Roster: []RosterEntry{{Session: hostile, Handle: hostile, PresenceSource: "notifier_live", CountError: hostile}},
		Items: []Item{{
			Ref: obligation, Title: hostile, State: StateNeedsHuman, Structural: true,
			Owner:     Owner{Kind: OwnerHuman, Handle: "user"},
			Requester: &agent, WaitsOn: []Owner{agent},
			Reason: Reason{Field: hostile, Text: hostile, Since: now.Add(-time.Hour)},
			Evidence: []Evidence{
				{Field: hostile, Value: hostile, Text: hostile, At: now, URL: "javascript:alert(1)"},
				{Field: hostile, Value: hostile, At: now, URL: "https://forge.example.net/o/r/pull/1"},
			},
			Obligation: &ObligationFacts{
				OpenerID: hostile, Thread: hostile, OwnerSession: hostile,
				Destination:    &Destination{Handle: hostile, Session: hostile, Project: hostile},
				CompletionKind: "answer", AcknowledgeKind: "status",
			},
		}},
	}
	beads := Result{Status: StatusOK, ObservedAt: now, Items: []Item{{
		Ref: bead, Title: hostile, State: StateActive, Structural: true, Owner: agent,
		Reason:    Reason{Field: hostile, Text: hostile, Since: now.Add(-48 * time.Hour)},
		Relations: []Relation{{Kind: RelationRelated, To: bead, Basis: hostile}},
		Evidence:  []Evidence{{Field: hostile, Value: hostile, Text: hostile, At: now}},
	}}}
	m := Build(Input{
		Results: []NamedResult{{Name: "amq", Result: amq}, {Name: "beads", Result: beads}, {Name: hostile, Result: Result{Status: StatusUnavailable, Detail: hostile}}},
		Known:   map[string]string{"demo-a1b": "in_progress"},
		Config:  DefaultConfig(), Now: now, Version: hostile, Dir: hostile,
	})
	cost := 1.5
	m.Usage = &UsageView{
		Fleet: []Usage{{Window: hostile, Unknown: hostile}},
		Accounts: []AccountUsage{{Key: hostile, Harness: hostile, Detail: hostile, OtherSessions: 1,
			Limits: []usage.Limit{{Kind: "unknown", Name: hostile, Reason: hostile}},
			Other:  []Usage{{Window: hostile, CostUSD: &cost, CostKind: hostile}}}},
	}
	m.Suggestions = []Suggestion{{
		Status: SuggestionNeedsReview,
		ID:     hostile, Unit: EvidenceUnit{MessageKey: hostile, Field: hostile, Quote: hostile + "\n\tSource continuation"},
		Choices: []SuggestionChoice{{Kind: "new", Outcome: &EvidenceUnit{Quote: hostile}}},
	}}
	m.SuggestionDiagnostic = SuggestionAttempt{Status: "unavailable", Detail: hostile}

	var page strings.Builder
	if err := WriteHTML(&page, m, Quote([]string{"amq-board", "--html", "--out", hostile})); err != nil {
		t.Fatal(err)
	}
	html := page.String()

	// Finish review of this page: a source both degraded and partial named
	// only its error in the empty state and dropped its coverage gap.
	for _, c := range m.Columns {
		if c.ID == ColumnBlocked && !strings.Contains(c.Empty, hostile+" gap") {
			t.Errorf("empty column hides the coverage gap: %q", c.Empty)
		}
	}

	const open = `<script type="application/json" id="model">`
	start := strings.Index(html, open)
	end := strings.Index(html[start:], "</script>")
	if start < 0 || end < 0 {
		t.Fatal("no embedded model")
	}
	embedded := html[start+len(open) : start+end]
	var got, want any
	if err := json.Unmarshal([]byte(embedded), &got); err != nil {
		t.Fatalf("embedded model does not parse: %v", err)
	}
	raw, _ := json.Marshal(buildPageModel(m))
	_ = json.Unmarshal(raw, &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("embedded model differs from the page model")
	}

	outside := html[:start] + html[start+end:]
	// Live Suggestions: terminal sanitization replaced source line breaks with '?'.
	if !strings.Contains(outside, "\n\tSource continuation</blockquote>") {
		t.Error("source quote lost its line breaks or tabs")
	}
	for _, bad := range []string{"<img", "onerror=alert(1)>", "</script><!--", "<!--", "\u2028"} {
		if strings.Contains(outside, bad) || strings.Contains(embedded, bad) {
			t.Errorf("page holds %q unescaped", bad)
		}
	}
	if strings.Contains(outside, "javascript:") {
		t.Error("page links a non-https URL")
	}
	// PR #1209 review: an https URL on a host that is not a configured forge
	// became a link; it must stay text.
	if strings.Contains(outside, `href="https://forge.example.net`) {
		t.Error("page links an https URL outside the configured forge hosts")
	}
	if !strings.Contains(outside, "&#34;&gt;&lt;img src=x onerror=alert(1)&gt;") {
		t.Error("hostile text is not rendered escaped")
	}
	for _, c := range m.Columns {
		for _, card := range c.Cards {
			for _, a := range card.Actions {
				if strings.Contains(a.Command, "img src") || strings.Contains(a.Command, "touch") {
					t.Errorf("command built from a hostile value: %s", a.Command)
				}
			}
		}
	}
	for _, rs := range m.Roster {
		for _, a := range append(rs.Agents, rs.Quiet...) {
			if a.Action != nil {
				t.Errorf("hostile handle got a command: %s", a.Action.Command)
			}
		}
	}
	if strings.Contains(outside, "--to &#39;&#34;&gt;&lt;img") || strings.Contains(outside, "--to &#39;\"&gt;") {
		t.Error("page offers a command for a hostile handle")
	}
}

// Live board, 2026-10-25: a missing age repeated its label. The age label
// appears once per rendering, including the Next and Requests views.
func TestHTMLUnknownStateAgeReadsOnce(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ref, _ := Obligation("s1/alpha/demo")
	alpha := Owner{Kind: OwnerAgent, Handle: "alpha", Session: "s1"}
	amq := Result{Status: StatusOK, ObservedAt: now, Feeds: []Column{ColumnWaiting}, Items: []Item{{
		Ref: ref, Title: "Pick the retry window", State: StateNeedsHuman, Structural: true,
		Owner: Owner{Kind: OwnerHuman, Handle: "user"}, Requester: &alpha, WaitsOn: []Owner{alpha},
		Reason: Reason{Field: "kind", Text: "todo owed by user"},
	}}}
	m := Build(Input{Results: []NamedResult{{Name: "amq", Result: amq}}, Config: DefaultConfig(), Now: now})
	// Live board: the header must retain publication age when a view is rendered later.
	m.PublishedAt = now.Add(-time.Hour)
	var page strings.Builder
	if err := WriteHTML(&page, m, "amq-board --html"); err != nil {
		t.Fatal(err)
	}
	if html := page.String(); strings.Contains(html, "Requested Requested") || strings.Count(html, "Requested unknown") != 2 {
		t.Error(`the missing request age must read "Requested unknown" once in each view`)
	}
	if !strings.Contains(page.String(), `datetime="2026-10-25T11:00:00Z"`) {
		t.Error("snapshot timestamp became the later view-render time")
	}
}

// A card face shows its one-sentence summary, and a conventional-commit
// prefix becomes a chip so the title starts with its noun.
func TestHTMLCardShowsSummaryAndPrefixChip(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	bead, _ := Bead("demo-a1b")
	beads := Result{Status: StatusOK, ObservedAt: now, Items: []Item{{
		Ref: bead, Title: "fix(board): coverage notes name real gaps", State: StateBlocked, Structural: true,
		Owner:  Owner{Kind: OwnerAgent, Handle: "alpha", Display: "alpha"},
		Reason: Reason{Field: "status", Text: "bead blocked", Since: now.Add(-time.Hour)},
	}}}
	m := Build(Input{Results: []NamedResult{{Name: "beads", Result: beads}}, Config: DefaultConfig(), Now: now})
	// Build places the card in a column; Flow stages come from projection,
	// so the card is staged here as Project would.
	for i := range m.Columns {
		for j := range m.Columns[i].Cards {
			c := &m.Columns[i].Cards[j]
			c.Summary, c.Stage = "Coverage notes name only the sources that missed records.", StageBlocked
			m.Flow = append(m.Flow, StageView{ID: StageBlocked, Title: StageBlocked.Title(), CardIDs: []string{c.ID}})
		}
	}
	var page strings.Builder
	if err := WriteHTML(&page, m, "amq-board --html"); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	if !strings.Contains(html, `<span class="card-summary">Coverage notes name only the sources that missed records.</span>`) {
		t.Error("the Flow card does not show its summary")
	}
	if !strings.Contains(html, `<span class="cc">fix(board)</span> Coverage notes name real gaps`) {
		t.Error("the title prefix is not a chip before the title")
	}
	// The Work row shows the summary in place of a reason that only restates
	// the state.
	if !strings.Contains(html, `<p class="item-summary">Coverage notes name only the sources that missed records.</p>`) || strings.Contains(html, `<p class="item-reason">bead blocked</p>`) {
		t.Error("the Work row does not replace the restated reason with its summary")
	}
}

// Every view names its coverage gaps in one compact disclosure whose body
// links to Sources. It says "Nothing for you to do" only when the board knows
// no owner step exists, offers a step it detects (a command as a copyable
// field), and otherwise says neither.
func TestHTMLCoverageNamesTheGaps(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	sources := `<p><a href="#view-sources">Sources</a> says why.</p></div></details>`
	rows := []struct {
		name   string
		result Result
		want   string
	}{
		{"beads", Result{Status: StatusOK, Coverage: Coverage{Unresolved: 238, Detail: "238 mentioned bead ids over the lookup cap"}},
			`<span class="gap"><span class="gap-name">beads</span> partly read</span></summary><div class="coverage-body"><div class="coverage-gap"><p><span class="gap-name">beads</span> partly read: 238 mentioned bead ids over the lookup cap.</p></div><p>Nothing for you to do.</p>` + sources},
		{"claude-tasks", Result{Status: StatusOK, Coverage: Coverage{Unresolved: 1, Detail: UnboundClaudeListGap("/h/.claude", "team-a")}},
			`<label class="cmd-label">Bind the Claude task list<textarea readonly rows="2" spellcheck="false">amq-board bind-claude-list --home /h/.claude --list team-a</textarea>`},
		// Review of the polish pass: a list or home holding a separator gave a
		// wrong copy-ready command; it now gets none and its gap still shows.
		{"claude-tasks", Result{Status: StatusOK, Coverage: Coverage{Unresolved: 1, Detail: UnboundClaudeListGap("/h/.claude", "a in b")}},
			`partly read: unbound native task list a in b in /h/.claude.</p></div>` + sources},
		{"codex-goals", Result{Status: StatusUnavailable, Detail: SQLite3Missing},
			`<p><span class="gap-name">codex-goals</span> unavailable.</p><p>Install sqlite3.</p></div>` + sources},
		{"github", Result{Status: StatusDegraded, Detail: "gh api graphql failed: HTTP 401"},
			`<p><span class="gap-name">github</span> read failed.</p></div>` + sources},
	}
	for _, row := range rows {
		row.result.ObservedAt = now
		m := Build(Input{Results: []NamedResult{{Name: row.name, Result: row.result}}, Config: DefaultConfig(), Now: now})
		var page strings.Builder
		if err := WriteHTML(&page, m, "amq-board --html"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(page.String(), row.want) {
			t.Errorf("%s: the coverage disclosure lacks %s", row.name, row.want)
		}
	}
}

// The Agents view lists sessions with a live wake or recent presence first
// and folds the rest into one closed disclosure at its end.
func TestHTMLAgentsFoldSessionsWithoutRecentPresence(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := Build(Input{Config: DefaultConfig(), Now: now})
	m.Roster = []RosterSession{
		{Session: "s1", Agents: []RosterAgent{{RosterEntry: RosterEntry{Session: "s1", Handle: "quietone"}, Liveness: "quiet"}}},
		{Session: "s2", Agents: []RosterAgent{{RosterEntry: RosterEntry{Session: "s2", Handle: "liveone"}, Liveness: "live"}}},
	}
	var page strings.Builder
	if err := WriteHTML(&page, m, "amq-board --html"); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	live, fold, quiet := strings.Index(html, "<h2>s2</h2>"), strings.Index(html, `<details class="context-block quiet-sessions"><summary>1 session without recent presence</summary>`), strings.Index(html, "<h2>s1</h2>")
	if live < 0 || fold < 0 || live >= fold || fold >= quiet {
		t.Errorf("Agents does not list the live session first and fold the quiet one after it (live %d, fold %d, quiet %d)", live, fold, quiet)
	}
}

// Each suggestion status renders its own label and is published as JSON.
func TestHTMLSuggestionStatusLabels(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	m := Build(Input{Config: DefaultConfig(), Now: now})
	rows := []struct {
		status SuggestionStatus
		label  string
	}{
		{SuggestionNeedsReview, "Needs your review"},
		{SuggestionInvalid, "No actionable commands"},
		{SuggestionStale, "Outdated; run suggestions again before adoption"},
		{SuggestionIgnored, "Ignored; source evidence retained"},
		{SuggestionAdopted, "Adopted"},
	}
	for _, r := range rows {
		m.Suggestions = []Suggestion{{ID: "s", Status: r.status}}
		var page strings.Builder
		if err := WriteHTML(&page, m, "amq-board --html"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(page.String(), r.label) {
			t.Errorf("status %s: page lacks label %q", r.status, r.label)
		}
		raw, err := json.Marshal(m.Suggestions[0])
		if err != nil || !strings.Contains(string(raw), `"status":"`+string(r.status)+`"`) {
			t.Errorf("status %s: JSON lacks status: %s", r.status, raw)
		}
	}
}

// The Inbox view lists a decision's options and one copy command per option,
// the recommended option first, then the free answer.
func TestHTMLInboxDecisionCopiesOneAnswerPerOption(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	answer := func(label, body string) Action {
		return newAction(label, "amq", "send", "--me", "user", "--to", "demo-lead", "--kind", "answer", "--body", body)
	}
	m := Model{GeneratedAt: now, Inbox: Inbox{Items: []OwnerItem{{
		ID: "demo/q1", Kind: "question", Title: "Pick a store", From: "demo-lead in demo", Since: now.Add(-time.Hour), Blocking: true,
		Options:     []OwnerOption{{Label: "Files", Pros: []string{"No dependency"}}, {Label: "Log", Cons: []string{"Needs a lock"}}},
		Recommended: "Log",
		Answer:      []Action{answer("answer Files", "Files"), answer("answer Log", "Log"), answer("complete: answer demo-lead", "-")},
	}}}}
	var page strings.Builder
	if err := WriteHTML(&page, m, ""); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	for _, want := range []string{
		`aria-label="Inbox, 1 waiting"`,
		"1 item waits on you, 1 blocks work.",
		`<dt>Log <span class="recommended">Recommended</span></dt>`,
		"<li>No dependency</li>", "<li>Needs a lock</li>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Inbox view lacks %q", want)
		}
	}
	log, files, free := strings.Index(html, ">Copy answer: Log</button>"), strings.Index(html, ">Copy answer: Files</button>"), strings.Index(html, ">Copy free answer</button>")
	if log < 0 || files < 0 || free < 0 || log > files || files > free {
		t.Errorf("answer buttons missing or out of order: Log %d, Files %d, free %d", log, files, free)
	}
}

// The Cost view lists each model row with its cost and share, in the order
// the usage view gives them.
func TestHTMLCostListsEachModelWithCostAndShare(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	cost := func(v float64) *float64 { return &v }
	window := func(w string, c float64) Usage {
		return Usage{Window: w, Fresh: 1000, CostUSD: cost(c), CostKind: usage.ListPrice}
	}
	m := Model{GeneratedAt: now, Usage: &UsageView{
		Fleet: []Usage{window("5h", 4), window("7d", 300)},
		FleetModels: []ModelUsage{
			{Model: "demo-large", Harness: "claude", Usage: []Usage{window("5h", 3), window("7d", 225)}},
			{Model: "demo-small", Harness: "codex", Usage: []Usage{window("5h", 1), window("7d", 75)}},
		},
	}}
	var page strings.Builder
	if err := WriteHTML(&page, m, ""); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	if !strings.Contains(html, "List-price equivalent: $4.00 in the last 5 h, $300.00 in 7 d.") {
		t.Error("Cost view lacks the fleet sentence")
	}
	large, small := strings.Index(html, `data-label="Model"><span class="cell-name">demo-large</span>`), strings.Index(html, `data-label="Model"><span class="cell-name">demo-small</span>`)
	if large < 0 || small < 0 || large > small {
		t.Errorf("model rows missing or out of the usage view's order: large %d, small %d", large, small)
	}
	if !strings.Contains(html, "$225.00") || !strings.Contains(html, "75.0%") || !strings.Contains(html, "25.0%") {
		t.Error("model rows lack their cost or share")
	}
}

// Design slice 1 (bead x36b.9): the board has one navigation bar on every
// view — Inbox with its count, Flow, Floor, Cost, then a closed More menu
// holding the secondary destinations and Appearance. The old left rail with
// its open More list and nav note is gone, and the footer says which writes
// the page can do on the face being served.
func TestHTMLNavigationBarIsOneBarWithAClosedMoreMenu(t *testing.T) {
	m := Model{}
	var page strings.Builder
	if err := WriteHTML(&page, m, ""); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	if strings.Count(html, `class="navigation"`) != 1 {
		t.Fatalf("the page holds more than one navigation bar")
	}
	if strings.Contains(html, "primary-nav") || strings.Contains(html, "nav-note") {
		t.Error("the navigation rail or its note is still on the page")
	}
	if strings.Contains(html, `<details class="secondary-nav" open>`) {
		t.Error("the More menu does not start closed")
	}
	navStart := strings.Index(html, `</header>`)
	navEnd := strings.Index(html, `<main id="content">`)
	if navStart < 0 || navEnd < 0 || navEnd < navStart {
		t.Fatal("the navigation bar does not sit between the header and the content")
	}
	bar := html[navStart:navEnd]
	for _, link := range []string{"inbox", "flow", "floor", "cost", "requests", "history", "sources", "suggestions", "older-requests", "agents"} {
		if !strings.Contains(bar, `data-view-link="`+link+`"`) {
			t.Errorf("the bar does not link the %s view", link)
		}
	}
	if !strings.Contains(bar, `id="appearance"`) {
		t.Error("Appearance is not in the More menu")
	}
	var live strings.Builder
	if err := WriteLiveHTML(&live, Model{}); err != nil {
		t.Fatal(err)
	}
	liveHTML := live.String()
	if !strings.Contains(liveHTML, "Answers you choose here are sent as you. Nothing else is run by this page.") {
		t.Error("the live footer does not say what the served page sends")
	}
	if !strings.Contains(html, "Commands are copied, never run by this page.") {
		t.Error("the static footer does not say the page only copies")
	}
}
