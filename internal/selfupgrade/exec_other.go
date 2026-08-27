//go:build !darwin && !linux

package selfupgrade

func execSupportedPlatform() bool { return false }

func execImagePlatform(ImageEvidence, []string, []string) error { return ErrExecUnsupported }
