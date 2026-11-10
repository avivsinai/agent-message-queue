package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/avivsinai/agent-message-queue/internal/cli"
)

var version = "dev"

func getVersion() string {
	// Prefer ldflags-injected version
	if version != "dev" {
		return version
	}
	// Fall back to Go module version (set by go install)
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	err := cli.Run(os.Args[1:], getVersion())
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
	}
	cli.WaitUpdateCheck()
	if err != nil {
		os.Exit(cli.GetExitCode(err))
	}
}
