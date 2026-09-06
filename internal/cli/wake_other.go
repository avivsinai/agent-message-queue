//go:build !darwin && !linux

package cli

import (
	"errors"
	"flag"
	"io"
)

func runWake(args []string) error {
	if len(args) > 0 && args[0] == "check" && wakeCheckV2OptInPresent(args[1:]) {
		return runWakeCheckUnsupported(args[1:])
	}
	return errors.New("amq wake is not supported on this platform (requires macOS or Linux)")
}

func wakeCheckV2OptInPresent(args []string) bool {
	fs := flag.NewFlagSet("wake check v2 opt-in", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOutput := fs.Bool("json", false, "")
	jsonSchema := fs.Int("json-schema", wakeCheckSchemaV1, "")
	_ = fs.String("root", "", "")
	_ = fs.String("me", "", "")
	_ = fs.Bool("strict", false, "")
	if err := fs.Parse(args); err != nil {
		return false
	}
	return *jsonOutput && *jsonSchema == wakeCheckSchemaV2
}
