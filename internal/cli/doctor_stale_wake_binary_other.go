//go:build !darwin && !linux

package cli

func inspectWakeBinaryStalenessPlatform(
	wakeLockInspection,
	resolvedWakeBinary,
) (wakeBinaryStaleness, error) {
	return wakeBinaryStaleness{}, nil
}
