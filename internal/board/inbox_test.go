package board

import (
	"encoding/json"
	"path/filepath"
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
		{"requester in the base root", valid, "", []string{"Today", "After the service"}, 4, nil},
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
				want := `(amq_context="$(amq env --root ` + filepath.ToSlash(filepath.Join("/base", tc.session)) + ` --me user --shell sh)" && eval "$amq_context" && printf '%s' ` + Quote([]string{label}) + " | amq reply --me user --id ask --kind answer --body -)"
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
