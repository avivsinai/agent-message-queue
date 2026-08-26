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
// agents must be 1 and inbound must be owner-only. The gate waits for the
// harness to hold a Buzz NIP-PL kind:30350 lease with a quota-1
// deployment-identity profile (block/buzz#5667); this companion does not
// treat an env string as one.
func PrepareWorkerEnv() error {
	agents := strings.TrimSpace(os.Getenv(envBuzzAgents))
	if agents != "" && agents != "1" {
		return fmt.Errorf("%s=%s: amq-acp accepts only agents=1 until the harness holds a Buzz NIP-PL kind:30350 lease with a quota-1 deployment-identity profile (block/buzz#5667)", envBuzzAgents, agents)
	}
	respondTo := strings.TrimSpace(os.Getenv(envBuzzRespondTo))
	if respondTo != "" && respondTo != "owner-only" {
		return fmt.Errorf("%s=%s: amq-acp accepts only owner-only inbound until the harness holds a Buzz NIP-PL kind:30350 lease with a quota-1 deployment-identity profile (block/buzz#5667)", envBuzzRespondTo, respondTo)
	}
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
