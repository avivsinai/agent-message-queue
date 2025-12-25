package main

import (
	"fmt"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/cli"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && isVersionArg(os.Args[1]) {
		if _, err := fmt.Fprintln(os.Stdout, version); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := cli.Run(os.Args[1:]); err != nil {
		if _, werr := fmt.Fprintln(os.Stderr, err); werr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func isVersionArg(arg string) bool {
	switch arg {
	case "--version", "-v", "version":
		return true
	default:
		return false
	}
}
