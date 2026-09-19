// Package main: adapters.go is the single place adapter factory packages are
// linked into the shipped amq-remote binary. Each package's init() calls
// registry.Register; serve has no adapter logic and no per-adapter imports.
// A future Amit factory is linked in here.
package main

import (
	_ "github.com/avivsinai/agent-message-queue/internal/remote/claude" // registers claude stub factory
	_ "github.com/avivsinai/agent-message-queue/internal/remote/codex"  // registers codex factory
	_ "github.com/avivsinai/agent-message-queue/internal/remote/fake"   // registers fake factory
)
