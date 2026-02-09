//go:build !darwin && !linux

package cli

import "errors"

func runCoopExec(args []string) error {
	return errors.New("amq coop exec is not supported on this platform (requires macOS or Linux)")
}
