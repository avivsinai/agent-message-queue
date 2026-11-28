package board

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// A question to the owner that carries context.decision is one Inbox item
// with its options and one answer command per option; a stored decision that
// breaks the shape gives a plain question item. Needs you counts the item.
// Pre-merge review of the Inbox: an item with no answer command did not say
// why, a past reply-by did not read overdue, and the mail command drained 20
// of 27 unread.
func TestInboxListsADecisionForTheOwner(t *testing.T) {
	const valid = `{"decision":{"options":[{"label":"Today","summary":"Ships the fixes now.","pros":["sooner"],"cons":["two upgrades"]},{"label":"After the service","summary":"One release."}],"recommended":"After the service","blocking":true,"if_later":"The release waits."}}`
	for _, tc := range []struct {
		name, context, session string
		options                []string
		answers                int
		text                   []string
	}{
		{"valid decision", valid, "s1", []string{"Today", "After the service"}, 4, []string{"reply by 2026-11-01T11:00:00Z (overdue)", "mail: s1 27 unread, oldest 24h · amq drain --me user --session s1 --limit 27 --include-body"}},
		{"invalid stored decision", strings.Replace(valid, `"recommended":"After the service"`, `"recommended":"Never"`, 1), "s1", nil, 2, nil},
		{"requester in the base root", valid, "", []string{"Today", "After the service"}, 0, []string{"no answer command: the requester is in the base root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask := requestMessage("ask", "question", "p2p/lead__user", "lead", []string{reservedHuman}, tc.session, requestNow.Add(-time.Hour))
			ask.ReplyBy = "2026-11-01T11:00:00Z"
			if err := json.Unmarshal([]byte(tc.context), &ask.Context); err != nil {
				t.Fatal(err)
			}
			s := requestSnapshot(ask)
			s.Input.Results[0].Result.AMQ.Gates = []Gate{{Session: "s1", OpenCount: 27, OldestAgeSeconds: 86400}}
			m := s.Project(DefaultConfig(), requestNow, "test")
			if len(m.Inbox.Items) != 1 {
				t.Fatalf("inbox items = %+v, want 1", m.Inbox.Items)
			}
			it := m.Inbox.Items[0]
			var labels []string
			for _, o := range it.Options {
				labels = append(labels, o.Label)
			}
			if !slices.Equal(labels, tc.options) || it.Blocking != (tc.options != nil) {
				t.Fatalf("options %v blocking %v, want %v", labels, it.Blocking, tc.options)
			}
			if tc.options != nil && it.Recommended != "After the service" {
				t.Errorf("recommended = %q", it.Recommended)
			}
			if len(it.Answer) != tc.answers || (tc.answers == 0) != (it.NoAnswer != "") {
				t.Fatalf("answers = %+v, no_answer %q, want %d", it.Answer, it.NoAnswer, tc.answers)
			}
			for i, label := range tc.options[:min(len(tc.options), tc.answers)] {
				want := "amq send --session s1 --me user --to lead --thread p2p/lead__user --refs ask --kind answer --body " + Quote([]string{label})
				if got := it.Answer[i].Command; got != want {
					t.Errorf("answer %d = %q, want %q", i, got, want)
				}
			}
			var text strings.Builder
			if err := WriteText(&text, m); err != nil {
				t.Fatal(err)
			}
			for _, want := range append([]string{"needs you 1", "== Inbox ==  1 waits on you"}, tc.text...) {
				if !strings.Contains(text.String(), want) {
					t.Errorf("text lacks %q:\n%s", want, text.String())
				}
			}
		})
	}
}

// Live board 2026-11-28: PR #1664 asked for the owner's review, was flagged
// Needs you on Flow, and appeared in neither the Inbox nor the count: the
// Inbox read only the Waiting column and needsYou only task cards. The Flow
// attention line, the Inbox headline and the navigation count must read the
// same number, and the PR must be an Inbox row.
func TestInboxCountsAFlaggedPullRequestTheSameAsTheHeadline(t *testing.T) {
	now := requestNow
	ref, _ := GitHubRepoRef("acme/app", 1664)
	pr := Item{Ref: ref, Title: "fix(wake): cancel a peer doorbell", State: StateNeedsHuman,
		WaitsOn: []Owner{{Kind: OwnerHuman, Display: "owner"}},
		Reason:  Reason{Field: "reviewRequests", Text: "your review requested", Since: now.Add(-15 * time.Hour)},
		Evidence: []Evidence{
			{Field: "type", Value: ForgePullRequest, Text: "PR #1664", URL: "https://github.com/acme/app/pull/1664"},
			{Field: "summary", Value: "A peer doorbell is cancelled twice."},
		}}
	s := requestSnapshot()
	s.Input.Results = append(s.Input.Results, NamedResult{Name: "github", Result: Result{Status: StatusOK, ObservedAt: now, Items: []Item{pr}}})
	m := s.Project(DefaultConfig(), now, "test")
	if len(m.Inbox.Items) != 1 {
		t.Fatalf("inbox items = %+v, want the flagged PR", m.Inbox.Items)
	}
	it := m.Inbox.Items[0]
	if it.Kind != "review_request" || it.Title != "Review #1664" || it.Link != "https://github.com/acme/app/pull/1664" {
		t.Fatalf("PR item = %+v", it)
	}
	headline := needsYou(m).count
	var text strings.Builder
	if err := WriteText(&text, m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "needs you 1") {
		t.Errorf("text header lacks \"needs you 1\":\n%s", text.String())
	}
	nav := buildInboxPage(m.Inbox, now).Count
	if headline != 1 || nav != 1 {
		t.Fatalf("attention=%d nav=%d, want 1 and 1", headline, nav)
	}
	page := buildInboxPage(m.Inbox, now)
	if len(page.Items) != 1 || page.Items[0].Link == "" {
		t.Fatalf("page items = %+v", page.Items)
	}
}

// A decision bead that no question names is an Inbox row that says the
// options have not been sent, with no answer command; the notifier reports
// it once as decision_without_options. A question whose context.bead names
// the task folds the task into the question row.
func TestInboxDecisionWithoutOptionsAndQuestionFold(t *testing.T) {
	const withBead = `{"decision":{"options":[{"label":"Go"},{"label":"Not yet"}],"recommended":"Go","blocking":true},"bead":"demo-9"}`
	for _, tc := range []struct {
		name, context string
		items         int
		wantSummary   string
		wantProblems  int
	}{
		{"no question names the task", "", 1, "The options have not been sent yet.", 1},
		{"a question names the task", withBead, 1, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := requestNow
			s := requestSnapshot()
			ref, _ := Bead("demo-9")
			alias := "beads:/work/.beads:demo-9"
			fact := Item{Source: "beads", Namespace: "/work/.beads", Ref: ref, Title: "demo-9", State: StateNeedsHuman, Structural: true,
				Owner:    Owner{Kind: OwnerAgent, Handle: "claude"},
				Reason:   Reason{Field: "issue_type", Text: "decision waits on you", Since: now.Add(-time.Hour)},
				Evidence: []Evidence{{Field: "issue_type", Value: "decision"}}}
			s.Tasks["task-decision"] = Task{ID: "task-decision", Title: "demo-9", Authority: alias, Aliases: []string{alias}}
			s.Facts[alias] = AliasFact{Alias: alias, Fact: fact, ObservedAt: now}
			if tc.context != "" {
				ask := requestMessage("ask", "question", "p2p/lead__user", "lead", []string{reservedHuman}, "s1", now.Add(-time.Hour))
				if err := json.Unmarshal([]byte(tc.context), &ask.Context); err != nil {
					t.Fatal(err)
				}
				s.Messages[ask.Key] = ask
			}
			m := s.Project(DefaultConfig(), now, "test")
			if len(m.Inbox.Items) != tc.items {
				t.Fatalf("inbox items = %+v, want %d", m.Inbox.Items, tc.items)
			}
			problems, err := Check(m, DefaultConfig(), now)
			if err != nil && tc.wantProblems == 0 && !strings.Contains(err.Error(), "no published snapshot") {
				t.Fatal(err)
			}
			if err == nil {
				withoutOptions := 0
				for _, p := range problems {
					if p.Kind == ProblemDecisionWithoutOptions {
						withoutOptions++
					}
				}
				if withoutOptions != tc.wantProblems {
					t.Fatalf("decision_without_options problems = %d, want %d (%+v)", withoutOptions, tc.wantProblems, problems)
				}
			}
			if tc.wantSummary != "" {
				var found bool
				for _, it := range m.Inbox.Items {
					if it.Summary == tc.wantSummary && len(it.Options) == 0 {
						found = true
					}
				}
				if !found {
					t.Fatalf("no item reads %q: %+v", tc.wantSummary, m.Inbox.Items)
				}
			}
		})
	}
}

// Mail for you splits at the obligation age: newer mail is one row per
// session with its read command, older mail is one folded line with no
// command, so old mail never shouts.
func TestInboxMailSplitsAtObligationAge(t *testing.T) {
	now := requestNow
	s := requestSnapshot()
	s.Input.Results[0].Result.AMQ.Gates = []Gate{
		{Session: "s1", OpenCount: 4, OldestAgeSeconds: (20 * time.Minute).Seconds()},
		{Session: "s2", OpenCount: 12, OldestAgeSeconds: (127 * 24 * time.Hour).Seconds()},
		{Session: "s3", OpenCount: 21, OldestAgeSeconds: (130 * 24 * time.Hour).Seconds()},
	}
	m := s.Project(DefaultConfig(), now, "test")
	in := m.Inbox
	if len(in.Mail) != 1 || in.Mail[0].Session != "s1" {
		t.Fatalf("mail rows = %+v, want only s1", in.Mail)
	}
	if in.OldMail == nil || in.OldMail.Sessions != 2 || in.OldMail.Unread != 33 {
		t.Fatalf("old mail = %+v, want 2 sessions, 33 unread", in.OldMail)
	}
	page := buildInboxPage(in, now)
	if page.OldMail == nil || page.OldMail.Oldest == "" || page.MaxAge == "" {
		t.Fatalf("page old mail = %+v max age %q", page.OldMail, page.MaxAge)
	}
	var text strings.Builder
	if err := WriteText(&text, m); err != nil {
		t.Fatal(err)
	}
	out := text.String()
	if !strings.Contains(out, "mail: s1 4 unread") {
		t.Errorf("text lacks the s1 mail row:\n%s", out)
	}
	if strings.Contains(out, "mail: s2") || strings.Contains(out, "mail: s3") {
		t.Errorf("old sessions still have their own mail lines:\n%s", out)
	}
}
