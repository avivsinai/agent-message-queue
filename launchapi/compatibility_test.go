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
	if slices.Contains(Compatibility().Features, "prepare_apply_v1") {
		for _, feature := range []string{
			FeaturePlacement, FeatureInitialInput, FeatureBaseRoot, FeatureOnLive,
			FeatureCallerContext, FeatureExecutableIdentity, FeatureWrapper,
		} {
			if !slices.Contains(Compatibility().Features, feature) {
				t.Fatalf("Compatibility omitted advertised %s", feature)
			}
		}
	}

	requirement := RequirementV1{
		ContractSemver: ">=0.61.0 <0.62.0",
		IntentVersion:  1,
		ResultVersion:  1,
		Features:       []string{"managed_tmux_v1", "launch_intent_v1"},
	}
	negotiated, err := Negotiate(requirement)
	if slices.Contains(Compatibility().Features, "managed_tmux_v1") {
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(negotiated.Features, []string{"launch_intent_v1", "managed_tmux_v1"}) {
			t.Fatalf("negotiated features = %v", negotiated.Features)
		}
	} else if err == nil || !strings.Contains(err.Error(), "unsupported required feature") {
		t.Fatalf("unsupported managed_tmux negotiation = %v", err)
	}
	placed, err := Negotiate(RequirementV1{
		ContractSemver: "0.61.1", IntentVersion: 1, ResultVersion: 1, Features: []string{FeaturePlacement},
	})
	if slices.Contains(Compatibility().Features, FeaturePlacement) {
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(placed.Features, []string{FeaturePlacement}) {
			t.Fatalf("negotiated placement = %v", placed.Features)
		}
	} else if err == nil || !strings.Contains(err.Error(), "unsupported required feature") {
		t.Fatalf("unsupported placement negotiation = %v", err)
	}

	if _, err := Negotiate(RequirementV1{ContractSemver: ">=0.61.0", IntentVersion: 1, ResultVersion: 1}); err != nil {
		t.Fatalf(">=0.61.0 must include the current contract: %v", err)
	}
	if _, err := Negotiate(RequirementV1{ContractSemver: "<=0.61.1", IntentVersion: 1, ResultVersion: 1}); err != nil {
		t.Fatalf("<=0.61.1 must include the current contract: %v", err)
	}
}
