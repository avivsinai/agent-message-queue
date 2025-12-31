//go:build !darwin && !linux

package cli

import "errors"

func runWake(args []string) error {
	return errors.New("amq wake is not supported on this platform (requires macOS or Linux)")
}
