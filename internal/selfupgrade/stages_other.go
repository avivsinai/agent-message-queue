//go:build !darwin

package selfupgrade

func cleanupImageStagesPlatform(string) error { return nil }
