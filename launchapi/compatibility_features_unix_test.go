//go:build darwin || linux

package launchapi

import (
	"reflect"
	"testing"
)

func TestUnixCompatibilityAdvertisesCallableLaunchSurface(t *testing.T) {
	want := []string{
		"launch_intent_v1", "prepare_apply_v1", "lifecycle_v1", "managed_tmux_v1", "plan_only_commands_v1",
		FeatureInitialInput, FeaturePlacement, FeatureBaseRoot, FeatureOnLive, FeatureCallerContext,
		FeatureExecutableIdentity, FeatureWrapper,
	}
	if got := Compatibility().Features; !reflect.DeepEqual(got, want) {
		t.Fatalf("Unix compatibility features = %v, want %v", got, want)
	}
}
