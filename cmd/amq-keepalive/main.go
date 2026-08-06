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
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
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
