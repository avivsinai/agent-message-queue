//go:build !darwin && !linux

package cli

func runTerminalRelayHelper([]string) error {
	return ActionRequiredError("terminal relay is unsupported on this platform")
}
