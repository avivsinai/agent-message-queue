package acp

import (
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

const (
	envBuzzAgents    = "BUZZ_ACP_AGENTS"
	envBuzzRespondTo = "BUZZ_ACP_RESPOND_TO"
)

// PrepareWorkerEnv enforces the Buzz remote-task policy on this process, then
// strips every BUZZ_* variable so secrets cannot leak into later work.
//
// Inbound must be owner-only. With AMQ_ACP_REMOTE=binding, Buzz may start any
// number of identical workers: every turn decides through per-event file
// locks, and one worker per Buzz identity leads the lifecycle (lifecycle.go).
// In BYOH mode (AMQ_ACP_TO or AMQ_ACP_REMOTE_TARGET), or with no advisory
// lock to elect that leader, amq-acp runs one worker, so agents must be 1.
func PrepareWorkerEnv() error {
	agents := strings.TrimSpace(os.Getenv(envBuzzAgents))
	if agents != "" && agents != "1" {
		switch {
		case strings.TrimSpace(os.Getenv(EnvRemote)) != RemoteModeBinding:
			return fmt.Errorf("%s=%s: amq-acp in BYOH mode runs one worker; set %s=1 (parallelism 1)", envBuzzAgents, agents, envBuzzAgents)
		case !lock.AdvisoryLockAvailable():
			return fmt.Errorf("%s=%s: amq-acp runs one worker on a platform without advisory locks; set %s=1 (parallelism 1)", envBuzzAgents, agents, envBuzzAgents)
		}
	}
	respondTo := strings.TrimSpace(os.Getenv(envBuzzRespondTo))
	if respondTo != "" && respondTo != "owner-only" {
		return fmt.Errorf("%s=%s: amq-acp answers only its owner; set %s=owner-only", envBuzzRespondTo, respondTo, envBuzzRespondTo)
	}
	// The managed agent's identity stays in process memory for posting
	// answers (post.go); the environment is still stripped, so no later
	// child and no AMQ message sees it.
	captureBuzzIdentity()
	captureBuzzAgent()
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
