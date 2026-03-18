// internal/resolve/address.go
package resolve

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	KindAgent   = "agent"
	KindChannel = "channel"
)

// Endpoint is the canonical internal representation of a qualified AMQ address.
type Endpoint struct {
	Kind    string // "agent" or "channel"
	Agent   string // for agent endpoints
	Channel string // for channel endpoints
	Session string // optional session qualifier
	Project string // optional project qualifier
}

var handleRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ParseAddress parses a user-facing AMQ address string into an Endpoint.
//
// Supported forms:
//
//	codex                           -> local agent
//	codex@auth                      -> agent in session "auth" (same project)
//	claude@infra-lib:auth           -> agent in project "infra-lib", session "auth"
//	claude@session/auth             -> explicit: agent in session "auth"
//	claude@project/infra-lib        -> explicit: agent in project "infra-lib"
//	claude@project/infra-lib/session/auth -> explicit: project + session
//	#events                         -> channel in current project
//	#all@infra-lib                  -> channel in project "infra-lib"
//	#session/auth                   -> channel scoped to session "auth"
//	#session/auth@infra-lib         -> channel scoped to session in project
//
// The short form agent@name is syntactically ambiguous between session and
// project. The parser stores it in Session (local-first convention); the
// resolver disambiguates at resolution time by checking local sessions first,
// then the project registry. Use agent@project:session or the explicit long
// forms for unambiguous addressing.
func ParseAddress(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("empty address")
	}

	// Channel addresses start with #
	if strings.HasPrefix(raw, "#") {
		return parseChannel(raw[1:])
	}

	return parseAgent(raw)
}

func parseAgent(raw string) (Endpoint, error) {
	// Split on @ to separate agent from qualifier
	parts := strings.SplitN(raw, "@", 2)
	agent := parts[0]
	if !handleRe.MatchString(agent) {
		return Endpoint{}, fmt.Errorf("invalid agent handle %q: must be lowercase alphanumeric, dash, or underscore", agent)
	}

	ep := Endpoint{Kind: KindAgent, Agent: agent}

	if len(parts) == 1 {
		return ep, nil // bare handle
	}

	qualifier := parts[1]
	if qualifier == "" {
		return Endpoint{}, fmt.Errorf("empty qualifier after @")
	}

	// Reject double-@ (already split on first @, but qualifier could contain @)
	if strings.Contains(qualifier, "@") {
		return Endpoint{}, fmt.Errorf("invalid qualifier %q: multiple @ not allowed", qualifier)
	}

	// Explicit long forms: session/X, project/X, project/X/session/Y
	if strings.HasPrefix(qualifier, "session/") {
		ep.Session = qualifier[len("session/"):]
		if !handleRe.MatchString(ep.Session) {
			return Endpoint{}, fmt.Errorf("invalid session name %q", ep.Session)
		}
		return ep, nil
	}
	if strings.HasPrefix(qualifier, "project/") {
		rest := qualifier[len("project/"):]
		if idx := strings.Index(rest, "/session/"); idx >= 0 {
			ep.Project = rest[:idx]
			ep.Session = rest[idx+len("/session/"):]
		} else {
			ep.Project = rest
		}
		if !handleRe.MatchString(ep.Project) {
			return Endpoint{}, fmt.Errorf("invalid project name %q", ep.Project)
		}
		if ep.Session != "" && !handleRe.MatchString(ep.Session) {
			return Endpoint{}, fmt.Errorf("invalid session name %q", ep.Session)
		}
		return ep, nil
	}

	// Short forms: agent@project:session
	if strings.Contains(qualifier, ":") {
		colonParts := strings.SplitN(qualifier, ":", 2)
		ep.Project = colonParts[0]
		ep.Session = colonParts[1]
		if !handleRe.MatchString(ep.Project) {
			return Endpoint{}, fmt.Errorf("invalid project name %q", ep.Project)
		}
		if !handleRe.MatchString(ep.Session) {
			return Endpoint{}, fmt.Errorf("invalid session name %q", ep.Session)
		}
		return ep, nil
	}

	// agent@name -- ambiguous: could be session or project.
	// Stored as-is; the resolver disambiguates at resolution time.
	// For now, store in Session (local-first resolution).
	// The resolver will check local sessions first, then project registry.
	if !handleRe.MatchString(qualifier) {
		return Endpoint{}, fmt.Errorf("invalid qualifier %q", qualifier)
	}
	ep.Session = qualifier
	return ep, nil
}

func parseChannel(raw string) (Endpoint, error) {
	if raw == "" {
		return Endpoint{}, fmt.Errorf("empty channel name after #")
	}

	ep := Endpoint{Kind: KindChannel}

	// Check for project qualifier: #channel@project
	if idx := strings.Index(raw, "@"); idx >= 0 {
		channelPart := raw[:idx]
		projectPart := raw[idx+1:]
		if !handleRe.MatchString(projectPart) {
			return Endpoint{}, fmt.Errorf("invalid project name %q in channel address", projectPart)
		}
		// Channel part may contain session/ prefix
		ep.Project = projectPart
		raw = channelPart
	}

	// Check for session-scoped channel: #session/auth
	if strings.HasPrefix(raw, "session/") {
		ep.Channel = "session"
		ep.Session = raw[len("session/"):]
		if !handleRe.MatchString(ep.Session) {
			return Endpoint{}, fmt.Errorf("invalid session name %q in channel", ep.Session)
		}
		return ep, nil
	}

	if !handleRe.MatchString(raw) {
		return Endpoint{}, fmt.Errorf("invalid channel name %q", raw)
	}
	ep.Channel = raw
	return ep, nil
}

// IsLocal returns true if the address targets the current session only.
func (e Endpoint) IsLocal() bool {
	return e.Session == "" && e.Project == ""
}

// IsCrossProject returns true if the address targets a different project.
func (e Endpoint) IsCrossProject() bool {
	return e.Project != ""
}

// IsCrossSession returns true if the address targets a different session (same project).
func (e Endpoint) IsCrossSession() bool {
	return e.Session != "" && e.Project == ""
}

// String returns the canonical string form of the endpoint.
func (e Endpoint) String() string {
	switch e.Kind {
	case KindChannel:
		s := "#" + e.Channel
		if e.Session != "" {
			s = "#" + e.Channel + "/" + e.Session
		}
		if e.Project != "" {
			s += "@" + e.Project
		}
		return s
	default: // agent
		s := e.Agent
		if e.Project != "" && e.Session != "" {
			s += "@" + e.Project + ":" + e.Session
		} else if e.Project != "" {
			s += "@" + e.Project
		} else if e.Session != "" {
			s += "@" + e.Session
		}
		return s
	}
}
