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
// escaped, the embedded model parses back equal, the page script builds no
// markup from strings, and a hostile handle yields no command.
func TestHTMLEscapesHostileTextAndRoundTripsTheModel(t *testing.T) {
	const hostile = `"><img src=x onerror=alert(1)>';touch /tmp/x;'</script><!--` + "\u2028"
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	bead, _ := Bead("demo-a1b")
	obligation, _ := Obligation("s1/alpha/demo")
	agent := Owner{Kind: OwnerAgent, Handle: hostile, Session: hostile, Display: hostile}
	amq := Result{
		Status: StatusDegraded, Detail: hostile, ObservedAt: now,
		Coverage: Coverage{Window: hostile, Detail: hostile},
		Feeds:    []Column{ColumnWaiting},
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
			Reason:   Reason{Field: hostile, Text: hostile, Since: now.Add(-time.Hour)},
			Evidence: []Evidence{{Field: hostile, Value: hostile, Text: hostile, At: now, URL: "javascript:alert(1)"}},
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

	var page strings.Builder
	if err := WriteHTML(&page, m, Quote([]string{"amq-board", "--html", "--out", hostile})); err != nil {
		t.Fatal(err)
	}
	html := page.String()

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
	raw, _ := json.Marshal(m)
	_ = json.Unmarshal(raw, &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("embedded model differs from the input model")
	}

	outside := html[:start] + html[start+end:]
	for _, bad := range []string{"<img", "onerror=alert(1)>", "</script><!--", "<!--", "\u2028"} {
		if strings.Contains(outside, bad) || strings.Contains(embedded, bad) {
			t.Errorf("page holds %q unescaped", bad)
		}
	}
	if strings.Contains(outside, "javascript:") {
		t.Error("page links a non-https URL")
	}
	if !strings.Contains(outside, "&#34;&gt;&lt;img src=x onerror=alert(1)&gt;") {
		t.Error("hostile text is not rendered escaped")
	}
	if strings.Contains(pageJS, "innerHTML") || strings.Contains(pageJS, "outerHTML") || strings.Contains(pageJS, "insertAdjacentHTML") || strings.Contains(pageJS, "document.write") {
		t.Error("page script builds markup from strings")
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
