//go:build !darwin && !linux

package selfupgrade

func execImagePlatform(ImageEvidence, []string, []string) error { return ErrExecUnsupported }
