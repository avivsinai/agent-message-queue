//go:build !darwin

package cli

// Off Darwin the version store is a no-op, so no wake needs to keep a stage.
func wakeRunsFromRestartStage() bool { return false }
