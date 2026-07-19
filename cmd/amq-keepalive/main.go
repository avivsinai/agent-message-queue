package main

import (
	"context"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/app"
)

func main() {
	os.Exit(app.App{}.Run(context.Background(), os.Args[1:]))
}
