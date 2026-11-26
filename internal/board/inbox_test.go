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
func TestInboxListsADecisionForTheOwner(t *testing.T) {
	const valid = `{"decision":{"options":[{"label":"Today","summary":"Ships the fixes now.","pros":["sooner"],"cons":["two upgrades"]},{"label":"After the service","summary":"One release."}],"recommended":"After the service","blocking":true,"if_later":"The release waits."}}`
	for _, tc := range []struct {
		name, context string
		options       []string
	}{
		{"valid decision", valid, []string{"Today", "After the service"}},
		{"invalid stored decision", strings.Replace(valid, `"recommended":"After the service"`, `"recommended":"Never"`, 1), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask := requestMessage("ask", "question", "p2p/lead__user", "lead", []string{reservedHuman}, "s1", requestNow.Add(-time.Hour))
			if err := json.Unmarshal([]byte(tc.context), &ask.Context); err != nil {
				t.Fatal(err)
			}
			m := requestSnapshot(ask).Project(DefaultConfig(), requestNow, "test")
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
			if len(it.Answer) != len(tc.options)+2 {
				t.Fatalf("answers = %+v, want one per option, then free text and acknowledge", it.Answer)
			}
			for i, label := range tc.options {
				want := "amq send --session s1 --me user --to lead --thread p2p/lead__user --refs ask --kind answer --body " + Quote([]string{label})
				if got := it.Answer[i].Command; got != want {
					t.Errorf("answer %d = %q, want %q", i, got, want)
				}
			}
			var text strings.Builder
			if err := WriteText(&text, m); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text.String(), "needs you 1") || !strings.Contains(text.String(), "== Inbox ==  1 waits on you") {
				t.Errorf("text headline or Inbox section:\n%s", text.String())
			}
		})
	}
}
