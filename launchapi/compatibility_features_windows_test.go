//go:build windows

package launchapi

import (
	"reflect"
	"strings"
	"testing"
)

func TestWindowsCompatibilityAdvertisesOnlyCallableLaunchSurface(t *testing.T) {
	want := []string{"launch_intent_v1", "plan_only_commands_v1"}
	if got := Compatibility().Features; !reflect.DeepEqual(got, want) {
		t.Fatalf("Windows compatibility features = %v, want %v", got, want)
	}
	for _, feature := range []string{"prepare_apply_v1", "lifecycle_v1", "managed_tmux_v1"} {
		_, err := Negotiate(RequirementV1{
			ContractSemver: ContractSemverV1, IntentVersion: IntentVersionV1, ResultVersion: ResultVersionV1,
			Features: []string{feature},
		})
		if err == nil || !strings.Contains(err.Error(), "unsupported required feature") {
			t.Fatalf("Windows negotiated %q: %v", feature, err)
		}
	}
}
