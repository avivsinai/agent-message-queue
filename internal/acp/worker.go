package acp

import (
	"fmt"
	"os"
	"strings"
)

const (
	envBuzzAgents    = "BUZZ_ACP_AGENTS"
	envBuzzRespondTo = "BUZZ_ACP_RESPOND_TO"
)

// PrepareWorkerEnv enforces the Buzz remote-task policy on this process, then
// strips every BUZZ_* variable so secrets cannot leak into later work.
//
// agents must be 1 and inbound must be owner-only. A lease-based gate that
// would relax this refusal waits on an upstream Buzz deployment-lease
// construct — upstream Buzz has no quota-1 deployment-lease profile today
// (bead agent-message-queue-1xl, premise corrected 2026-09-08: block/buzz
// #5667 is the ghost-key RFC and names no NIP-PL kind:30350 lease); this
// companion does not treat an env string as one.
func PrepareWorkerEnv() error {
	agents := strings.TrimSpace(os.Getenv(envBuzzAgents))
	if agents != "" && agents != "1" {
		return fmt.Errorf("%s=%s: amq-acp accepts only agents=1; a lease-based gate would relax this, but upstream Buzz has no deployment-lease profile today (tracked in bead agent-message-queue-1xl)", envBuzzAgents, agents)
	}
	respondTo := strings.TrimSpace(os.Getenv(envBuzzRespondTo))
	if respondTo != "" && respondTo != "owner-only" {
		return fmt.Errorf("%s=%s: amq-acp accepts only owner-only inbound; a lease-based gate would relax this, but upstream Buzz has no deployment-lease profile today (tracked in bead agent-message-queue-1xl)", envBuzzRespondTo, respondTo)
	}
	// The managed agent's identity stays in process memory for posting
	// answers (post.go); the environment is still stripped, so no later
	// child and no AMQ message sees it.
	captureBuzzIdentity()
	stripBuzzSecrets()
	return nil
}

func stripBuzzSecrets() {
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			name = entry
		}
		if strings.HasPrefix(name, "BUZZ_") {
			_ = os.Unsetenv(name)
		}
	}
}
