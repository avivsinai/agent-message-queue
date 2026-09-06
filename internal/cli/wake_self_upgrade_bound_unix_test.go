//go:build darwin || linux

package cli

const (
	wakeSelfUpgradeBoundProbeVersionEnv = "AMQ_TEST_WAKE_SELF_UPGRADE_BOUND_PROBE_VERSION"
	wakeSelfUpgradeBoundProbeModeEnv    = "AMQ_TEST_WAKE_SELF_UPGRADE_BOUND_PROBE_MODE"
	wakeSelfUpgradeBoundProbeFailMode   = "fail"
	wakeSelfUpgradeBoundProbeMutateMode = "mutate-after-version"
	wakeSelfUpgradeBoundProbeForkMode   = "fork-with-stdout"
)
