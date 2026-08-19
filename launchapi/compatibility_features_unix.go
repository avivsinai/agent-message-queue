//go:build darwin || linux

package launchapi

// platformCompatibilityFeaturesV1 is the callable launch surface on macOS
// and Linux. Keep this order stable: it is the canonical negotiation order.
func platformCompatibilityFeaturesV1() []string {
	return []string{
		"launch_intent_v1",
		"prepare_apply_v1",
		"lifecycle_v1",
		"managed_tmux_v1",
		"plan_only_commands_v1",
		FeatureInitialInput,
		FeaturePlacement,
		FeatureBaseRoot,
		FeatureOnLive,
		FeatureCallerContext,
		FeatureExecutableIdentity,
		FeatureWrapper,
	}
}
