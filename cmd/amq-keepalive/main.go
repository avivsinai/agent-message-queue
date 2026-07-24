package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/app"
)

func main() {
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
