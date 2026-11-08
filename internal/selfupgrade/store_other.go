//go:build !darwin

package selfupgrade

// RunFromStore is a no-op off Darwin. A Linux process keeps running, and is
// still attributed, after a package manager deletes its image.
func RunFromStore(string) error { return nil }

// InStore reports false off Darwin, where AMQ keeps no version store.
func InStore(string) bool { return false }
