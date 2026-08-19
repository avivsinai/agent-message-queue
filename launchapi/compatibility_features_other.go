//go:build !darwin && !linux && !windows

package launchapi

// Other platforms have the same fail-closed surface as native Windows until
// their callable launch implementations are available.
func platformCompatibilityFeaturesV1() []string {
	return []string{
		"launch_intent_v1",
		"plan_only_commands_v1",
	}
}
