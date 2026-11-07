package main

// The deterministic fake runtime is linked into test binaries only; the
// shipped binary registers no "fake" factory.
import _ "github.com/avivsinai/agent-message-queue/internal/remote/fake" // registers fake factory
