//go:build windows

package launchapi

// Native Windows can expose intent decoding and the plan-only commands
// contract. coop exec, managed execution, leases, and lifecycle backends are
// not callable there, so their execution-backed features are not advertised.
func platformCompatibilityFeaturesV1() []string {
	return []string{
		"launch_intent_v1",
		"plan_only_commands_v1",
	}
}
