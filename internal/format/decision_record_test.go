package format

import (
	"encoding/json"
	"strings"
	"testing"
)

// A decision question may name its door and the task it is about; a record
// of the option taken carries decided. The board folds a task's own row into
// the question that names it, so bead is validated by shape only.
func TestParseDecisionDoorBeadAndRecord(t *testing.T) {
	const base = `"options":[{"label":"On"},{"label":"Off"}],"blocking":true`
	for _, tc := range []struct{ name, field, patch string }{
		{"door two-way", "", `"door":"two-way"`},
		{"door one-way blocking", "", `"door":"one-way"`},
		{"record of the option taken", "", `"decided":"On"`},
		{"bead beside the decision", "", `"bead":"agent-message-queue-x36b.2"`},
		{"door not a known value", "context.decision.door", `"door":"sideways"`},
		{"door not a string", "context.decision.door", `"door":true`},
		{"one-way door not blocking", "context.decision.blocking", `"door":"one-way","blocking":false`},
		{"decided not among the labels", "context.decision.decided", `"decided":"Maybe"`},
		{"decided not a string", "context.decision.decided", `"decided":true`},
		{"bead not shaped like a task id", "context.decision.bead", `"bead":"Not A Task"`},
		{"bead with a control character", "context.decision.bead", `"bead":"a\u2028b-1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"decision":{` + base + `,` + tc.patch + `}}`
			var context map[string]any
			if err := json.Unmarshal([]byte(raw), &context); err != nil {
				t.Fatal(err)
			}
			_, ok, err := ParseDecision(context)
			if !ok {
				t.Fatal("decision key not found")
			}
			if tc.field == "" {
				if err != nil {
					t.Fatalf("valid decision refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err = %v, want one naming %s", err, tc.field)
			}
		})
	}
}

// A decision record with decided on a one-way door is refused at read: an
// agent cannot decide a one-way door, and the writer refuses it before
// delivery.
func TestParseDecisionRefusesDecidedOneWayDoor(t *testing.T) {
	var context map[string]any
	raw := `{"decision":{"options":[{"label":"Go"},{"label":"Not yet"}],"door":"one-way","blocking":true,"decided":"Go"}}`
	if err := json.Unmarshal([]byte(raw), &context); err != nil {
		t.Fatal(err)
	}
	_, _, err := ParseDecision(context)
	if err == nil || !strings.Contains(err.Error(), "context.decision.decided") {
		t.Fatalf("err = %v, want one naming context.decision.decided", err)
	}
}

// IsDecisionRecord marks the messages that record a taken option: they are
// records, not requests.
func TestIsDecisionRecord(t *testing.T) {
	record := map[string]any{"decision": map[string]any{"options": []any{map[string]any{"label": "On"}, map[string]any{"label": "Off"}}, "decided": "On"}}
	question := map[string]any{"decision": map[string]any{"options": []any{map[string]any{"label": "On"}, map[string]any{"label": "Off"}}}}
	if !IsDecisionRecord(record) {
		t.Error("a decision with decided is a record")
	}
	if IsDecisionRecord(question) {
		t.Error("a question is not a record")
	}
	if IsDecisionRecord(map[string]any{}) {
		t.Error("no decision key is not a record")
	}
}
