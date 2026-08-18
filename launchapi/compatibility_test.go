package launchapi

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCompatibilityAndNegotiateV1(t *testing.T) {
	compatibility := Compatibility()
	if compatibility.ContractSemver != "0.61.1" ||
		!reflect.DeepEqual(compatibility.IntentVersions, []int{1}) ||
		!reflect.DeepEqual(compatibility.ResultVersions, []int{1}) {
		t.Fatalf("Compatibility() = %#v", compatibility)
	}
	compatibility.Features[0] = "mutated"
	if Compatibility().Features[0] != "launch_intent_v1" {
		t.Fatal("Compatibility returned shared mutable feature storage")
	}
	if !slices.Contains(Compatibility().Features, FeaturePlacement) {
		t.Fatal("Compatibility omitted advertised placement")
	}
	if !slices.Contains(Compatibility().Features, FeatureInitialInput) {
		t.Fatal("Compatibility did not advertise the completed initial_input feature")
	}
	if !slices.Contains(Compatibility().Features, FeatureBaseRoot) {
		t.Fatal("Compatibility did not advertise the completed base_root feature")
	}
	if !slices.Contains(Compatibility().Features, FeatureOnLive) {
		t.Fatal("Compatibility omitted advertised on_live")
	}
	if !slices.Contains(Compatibility().Features, FeatureCallerContext) {
		t.Fatal("Compatibility did not advertise the completed caller_context feature")
	}
	if !slices.Contains(Compatibility().Features, FeatureExecutableIdentity) {
		t.Fatal("Compatibility omitted advertised executable_identity")
	}
	for _, unfinished := range []string{
		FeatureWrapper,
	} {
		if slices.Contains(Compatibility().Features, unfinished) {
			t.Fatalf("Compatibility advertised unfinished feature %q", unfinished)
		}
	}

	negotiated, err := Negotiate(RequirementV1{
		ContractSemver: ">=0.61.0 <0.62.0",
		IntentVersion:  1,
		ResultVersion:  1,
		Features:       []string{"managed_tmux_v1", "launch_intent_v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(negotiated.Features, []string{"launch_intent_v1", "managed_tmux_v1"}) {
		t.Fatalf("negotiated features = %v", negotiated.Features)
	}
	placed, err := Negotiate(RequirementV1{
		ContractSemver: "0.61.1", IntentVersion: 1, ResultVersion: 1, Features: []string{FeaturePlacement},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(placed.Features, []string{FeaturePlacement}) {
		t.Fatalf("negotiated placement = %v", placed.Features)
	}

	if _, err := Negotiate(RequirementV1{ContractSemver: ">=0.61.0", IntentVersion: 1, ResultVersion: 1}); err != nil {
		t.Fatalf(">=0.61.0 must include the current contract: %v", err)
	}
	if _, err := Negotiate(RequirementV1{ContractSemver: "<=0.61.1", IntentVersion: 1, ResultVersion: 1}); err != nil {
		t.Fatalf("<=0.61.1 must include the current contract: %v", err)
	}
}

func TestNegotiateV1FailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		requirement RequirementV1
		want        string
	}{
		{name: "older contract", requirement: RequirementV1{ContractSemver: "<0.61.0", IntentVersion: 1, ResultVersion: 1}, want: "does not include"},
		{name: "malformed range", requirement: RequirementV1{ContractSemver: "^0.61", IntentVersion: 1, ResultVersion: 1}, want: "does not include"},
		{name: "intent version", requirement: RequirementV1{ContractSemver: "0.61.1", IntentVersion: 2, ResultVersion: 1}, want: "intent version"},
		{name: "result version", requirement: RequirementV1{ContractSemver: "0.61.1", IntentVersion: 1, ResultVersion: 2}, want: "result version"},
		{name: "unknown feature", requirement: RequirementV1{ContractSemver: "0.61.1", IntentVersion: 1, ResultVersion: 1, Features: []string{"shell_hooks_v1"}}, want: "unsupported required feature"},
		{name: "duplicate feature", requirement: RequirementV1{ContractSemver: "0.61.1", IntentVersion: 1, ResultVersion: 1, Features: []string{"launch_intent_v1", "launch_intent_v1"}}, want: "duplicate required feature"},
		{name: "strict greater than current contract", requirement: RequirementV1{ContractSemver: ">0.61.1", IntentVersion: 1, ResultVersion: 1}, want: "does not include"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Negotiate(test.requirement); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Negotiate error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompareSemanticVersionsOrdersPatch(t *testing.T) {
	left, ok := parseSemanticVersion("1.0.1")
	if !ok {
		t.Fatal("parse 1.0.1")
	}
	right, ok := parseSemanticVersion("1.0.2")
	if !ok {
		t.Fatal("parse 1.0.2")
	}
	if cmp := compareSemanticVersions(left, right); cmp >= 0 {
		t.Fatalf("compare(1.0.1, 1.0.2) = %d, want negative", cmp)
	}
	if cmp := compareSemanticVersions(right, left); cmp <= 0 {
		t.Fatalf("compare(1.0.2, 1.0.1) = %d, want positive", cmp)
	}
}

func TestSemverRangeContainsRejectsRequirementWhitespace(t *testing.T) {
	if semverRangeContains(" >=0.61.0", "0.61.0") {
		t.Fatal("leading whitespace in the requirement must not match")
	}
}
