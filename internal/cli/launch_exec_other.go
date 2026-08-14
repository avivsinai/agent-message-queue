//go:build !darwin && !linux

package cli

func runLaunchExec([]string) error {
	return ActionRequiredError("managed launch execution acknowledgement is unsupported on this platform")
}
