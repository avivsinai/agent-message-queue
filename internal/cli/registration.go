package cli

import "github.com/avivsinai/agent-message-queue/internal/roster"

type (
	registrationStatus    = roster.Status
	agentDeclarations     = roster.Declarations
	agentRegistration     = roster.Registration
	agentRegistrationJSON = roster.RegistrationJSON
)

const (
	registrationDeclared     = roster.Declared
	registrationObserved     = roster.Observed
	registrationUnregistered = roster.Unregistered
)
