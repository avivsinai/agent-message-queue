package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/agentidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// registrationStatus says how AMQ knows a handle in one delivery root.
// Declared outranks observed, which outranks unregistered. It is independent
// of whether the handle's mailbox exists or is complete, and it never grants
// authority: delivery, repair and resume keep their own checks.
type registrationStatus string

const (
	// registrationDeclared: listed in the applicable base or session roster,
	// or the reserved human handle.
	registrationDeclared registrationStatus = "declared"
	// registrationObserved: not declared, but the mailbox holds a valid
	// identity record left by a harness that ran under the handle.
	registrationObserved registrationStatus = "observed"
	// registrationUnregistered: neither declared nor observed, for example a
	// mailbox a typo send created.
	registrationUnregistered registrationStatus = "unregistered"
)

// agentRoster is one declaration source, meta/config.json agents, read
// without side effects. Present is false only when the file does not exist;
// any other read or parse failure is kept in Err, never treated as "declares
// nothing".
type agentRoster struct {
	Path    string
	Present bool
	Agents  []string
	Err     error
}

func readAgentRoster(root string) agentRoster {
	path := filepath.Join(root, "meta", "config.json")
	data, err := fsq.ReadRegularNoFollow(path)
	return parseAgentRoster(path, data, err)
}

func parseAgentRoster(path string, data []byte, readErr error) agentRoster {
	roster := agentRoster{Path: path}
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return roster
		}
		roster.Present = true
		roster.Err = fmt.Errorf("cannot read %s: %w", path, readErr)
		return roster
	}
	roster.Present = true
	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		roster.Err = fmt.Errorf("invalid %s: %w", path, err)
		return roster
	}
	roster.Agents = cfg.Agents
	return roster
}

// agentDeclarations is the union of the rosters that apply to one delivery
// root: the base roster and the root's own roster. A session roster adds
// handles; it never removes a base declaration.
type agentDeclarations []agentRoster

// declarationsFor reads the rosters that apply to deliveryRoot. base is the
// already-resolved base root (from classifyRoot or an authenticated pin,
// never a display fallback); "" or base == deliveryRoot means the root is its
// own base.
func declarationsFor(deliveryRoot, base string) agentDeclarations {
	decls := agentDeclarations{}
	if base != "" && filepath.Clean(base) != filepath.Clean(deliveryRoot) {
		decls = append(decls, readAgentRoster(base))
	}
	return append(decls, readAgentRoster(deliveryRoot))
}

func (d agentDeclarations) declares(handle string) bool {
	if handle == reservedHumanHandle {
		return true
	}
	for _, roster := range d {
		for _, agent := range roster.Agents {
			if agent == handle {
				return true
			}
		}
	}
	return false
}

// err joins the failures of rosters that exist but cannot be read, so a
// strict caller can refuse instead of judging from a partial union.
func (d agentDeclarations) err() error {
	var errs []error
	for _, roster := range d {
		if roster.Err != nil {
			errs = append(errs, roster.Err)
		}
	}
	return errors.Join(errs...)
}

// agentRegistration is the classifier's verdict for one (delivery root,
// handle). Identity is set whenever a valid record exists, also for a
// declared handle. Diagnostics holds what kept the verdict from being
// complete: an unreadable roster when the handle is not otherwise declared,
// or an identity record that exists but is invalid.
type agentRegistration struct {
	Handle      string
	Status      registrationStatus
	Kind        string
	Identity    *agentidentity.Record
	Diagnostics []error
}

// classifyAgentRegistration is the one registration classifier. identityErr
// is the result of reading the handle's identity record in the same delivery
// root: nil with rec, a not-exist error for "no record", or any other error
// for a record that cannot be trusted.
func classifyAgentRegistration(decls agentDeclarations, handle string, rec agentidentity.Record, identityErr error) agentRegistration {
	reg := agentRegistration{Handle: handle, Kind: "agent", Status: registrationUnregistered}
	switch {
	case identityErr == nil:
		reg.Identity = &rec
	case !errors.Is(identityErr, os.ErrNotExist):
		reg.Diagnostics = append(reg.Diagnostics, identityErr)
	}
	switch {
	case handle == reservedHumanHandle:
		reg.Kind = "human"
		reg.Status = registrationDeclared
	case decls.declares(handle):
		reg.Status = registrationDeclared
	default:
		if reg.Identity != nil {
			reg.Status = registrationObserved
		}
		if err := decls.err(); err != nil {
			reg.Diagnostics = append(reg.Diagnostics, err)
		}
	}
	return reg
}

// classifyAgentAt classifies handle in deliveryRoot, reading its identity
// record from that root's mailbox.
func classifyAgentAt(decls agentDeclarations, deliveryRoot, handle string) agentRegistration {
	rec, err := agentidentity.Read(deliveryRoot, handle)
	return classifyAgentRegistration(decls, handle, rec, err)
}

// agentRegistrationJSON is the additive projection every agent listing
// embeds. It never reuses an existing field: kind, status and last_seen keep
// their meanings, and a declared handle still shows its observed identity.
type agentRegistrationJSON struct {
	RegistrationStatus      registrationStatus `json:"registration_status"`
	Identity                *agentIdentityJSON `json:"identity,omitempty"`
	RegistrationDiagnostics []string           `json:"registration_diagnostics,omitempty"`
}

type agentIdentityJSON struct {
	Harness    string `json:"harness"`
	Wrapper    string `json:"wrapper,omitempty"`
	ObservedAt string `json:"observed_at"`
}

func (r agentRegistration) json() agentRegistrationJSON {
	out := agentRegistrationJSON{RegistrationStatus: r.Status}
	if r.Identity != nil {
		out.Identity = &agentIdentityJSON{
			Harness:    r.Identity.Harness,
			Wrapper:    r.Identity.Wrapper,
			ObservedAt: r.Identity.ObservedAt,
		}
	}
	for _, err := range r.Diagnostics {
		out.RegistrationDiagnostics = append(out.RegistrationDiagnostics, err.Error())
	}
	return out
}
