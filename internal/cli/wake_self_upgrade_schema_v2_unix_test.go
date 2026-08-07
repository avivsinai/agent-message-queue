//go:build darwin || linux

package cli

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRenderWakeCheckV2SelfUpgradeFrozenFields(t *testing.T) {
	locator := "/opt/homebrew/bin/amq"
	got := renderWakeCheckV2(wakeCheckDecision{
		SelfUpgrade: wakeCheckSelfUpgradeDecision{
			Enabled:  true,
			Eligible: true,
			Locator:  &locator,
			LastCandidate: &wakeCheckSelfUpgradeCandidateDecision{
				Identity: "darwin:1:2:sha256:abc",
				Version:  "0.58.0",
			},
			LastDecision: &wakeCheckSelfUpgradeLastDecision{
				Action: "refused",
				Reason: "candidate is not newer",
				At:     "2026-08-07T12:00:00Z",
			},
			RefusedMemory: true,
		},
	})

	want := wakeCheckSelfUpgradeV2{
		Enabled:  true,
		Eligible: true,
		Locator:  &locator,
		LastCandidate: &wakeCheckSelfUpgradeCandidateV2{
			Identity: "darwin:1:2:sha256:abc",
			Version:  "0.58.0",
		},
		LastDecision: &wakeCheckSelfUpgradeLastDecisionV2{
			Action: "refused",
			Reason: "candidate is not newer",
			At:     "2026-08-07T12:00:00Z",
		},
		RefusedMemory: true,
	}
	if !reflect.DeepEqual(got.SelfUpgrade, want) {
		t.Fatalf("self upgrade = %#v, want %#v", got.SelfUpgrade, want)
	}

	raw, err := json.Marshal(got.SelfUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fields, map[string]any{
		"enabled":  true,
		"eligible": true,
		"locator":  locator,
		"last_candidate": map[string]any{
			"identity": "darwin:1:2:sha256:abc",
			"version":  "0.58.0",
		},
		"last_decision": map[string]any{
			"action": "refused",
			"reason": "candidate is not newer",
			"at":     "2026-08-07T12:00:00Z",
		},
		"refused_memory": true,
	}) {
		t.Fatalf("self-upgrade JSON fields = %#v", fields)
	}
}

func TestRenderWakeCheckV2SelfUpgradeZeroShape(t *testing.T) {
	got := renderWakeCheckV2(wakeCheckDecision{})
	raw, err := json.Marshal(got.SelfUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fields, map[string]any{
		"enabled":        false,
		"eligible":       false,
		"locator":        nil,
		"last_candidate": nil,
		"last_decision":  nil,
		"refused_memory": false,
	}) {
		t.Fatalf("zero self-upgrade JSON fields = %#v", fields)
	}
}
