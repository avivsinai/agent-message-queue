package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/app"
)

var version = "dev"

func getVersion() string {
	info, ok := debug.ReadBuildInfo()
	return resolveVersion(version, info, ok)
}

func resolveVersion(stamp string, info *debug.BuildInfo, ok bool) string {
	if stamp != "" && stamp != "dev" {
		return stamp
	}
	if !ok || info == nil {
		return "dev"
	}
	resolved := info.Main.Version
	if resolved == "" || resolved == "(devel)" {
		resolved = "dev"
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for _, key := range []string{"vcs.revision", "vcs.modified"} {
		if value := settings[key]; value != "" {
			resolved += fmt.Sprintf(" %s=%s", key, value)
		}
	}
	return resolved
}

func main() {
	if isVersionArgument(os.Args[1:]) {
		_, _ = fmt.Fprintln(os.Stdout, getVersion())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		// Restore default handling before cancellation propagates. A second
		// SIGINT/SIGTERM can therefore terminate a stuck shutdown normally.
		signal.Stop(signals)
		cancel()
	}()
	code := app.App{}.Run(ctx, os.Args[1:])
	signal.Stop(signals)
	cancel()
	os.Exit(code)
}

func isVersionArgument(args []string) bool {
	return len(args) == 1 && (args[0] == "-v" || args[0] == "--version" || args[0] == "version")
}
