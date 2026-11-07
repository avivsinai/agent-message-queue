//go:build !darwin

package cli

var runFromVersionStore = func(string) error { return nil }

func runWakeFromVersionStore() {}
