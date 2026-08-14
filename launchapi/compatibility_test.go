package launchapi

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompatibilityAndNegotiateV1(t *testing.T) {
	compatibility := Compatibility()
	if compatibility.ContractSemver != "0.61.0" ||
		!reflect.DeepEqual(compatibility.IntentVersions, []int{1}) ||
		!reflect.DeepEqual(compatibility.ResultVersions, []int{1}) {
		t.Fatalf("Compatibility() = %#v", compatibility)
	}
	compatibility.Features[0] = "mutated"
	if Compatibility().Features[0] != "launch_intent_v1" {
		t.Fatal("Compatibility returned shared mutable feature storage")
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
}

func TestNegotiateV1FailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		requirement RequirementV1
		want        string
	}{
		{name: "older contract", requirement: RequirementV1{ContractSemver: "<0.61.0", IntentVersion: 1, ResultVersion: 1}, want: "does not include"},
		{name: "malformed range", requirement: RequirementV1{ContractSemver: "^0.61", IntentVersion: 1, ResultVersion: 1}, want: "does not include"},
		{name: "intent version", requirement: RequirementV1{ContractSemver: "0.61.0", IntentVersion: 2, ResultVersion: 1}, want: "intent version"},
		{name: "result version", requirement: RequirementV1{ContractSemver: "0.61.0", IntentVersion: 1, ResultVersion: 2}, want: "result version"},
		{name: "unknown feature", requirement: RequirementV1{ContractSemver: "0.61.0", IntentVersion: 1, ResultVersion: 1, Features: []string{"shell_hooks_v1"}}, want: "unsupported required feature"},
		{name: "duplicate feature", requirement: RequirementV1{ContractSemver: "0.61.0", IntentVersion: 1, ResultVersion: 1, Features: []string{"launch_intent_v1", "launch_intent_v1"}}, want: "duplicate required feature"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Negotiate(test.requirement); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Negotiate error = %v, want %q", err, test.want)
			}
		})
	}
}
