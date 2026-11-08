// Package roster classifies how AMQ knows a handle in one delivery root:
// declared in a roster, observed through an identity record, or
// unregistered. It is discovery only and never grants authority.
package roster

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/agentidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/route"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// Status says how AMQ knows a handle in one delivery root.
// Declared outranks observed, which outranks unregistered. It is independent
// of whether the handle's mailbox exists or is complete, and it never grants
// authority: delivery, repair and resume keep their own checks.
type Status string

const (
	// Declared: listed in the applicable base or session roster,
	// or the reserved human handle.
	Declared Status = "declared"
	// Observed: not declared, but the mailbox holds a valid
	// identity record left by a harness that ran under the handle.
	Observed Status = "observed"
	// Unregistered: neither declared nor observed, for example a
	// mailbox a typo send created.
	Unregistered Status = "unregistered"
)

// Roster is one declaration source, meta/config.json agents, read
// without side effects. Present is false only when the file does not exist;
// any other read or parse failure is kept in Err, never treated as "declares
// nothing".
type Roster struct {
	Path    string
	Present bool
	Agents  []string
	Err     error
}

// Read reads the roster of root.
func Read(root string) Roster {
	path := filepath.Join(root, "meta", "config.json")
	data, err := fsq.ReadRegularNoFollow(path)
	return parse(path, data, err)
}

// ReadDeliveryRoot reads the roster of the delivery root an open capability
// holds.
func ReadDeliveryRoot(root *fsq.DeliveryRoot) Roster {
	data, err := root.ReadRegularNoFollow(filepath.Join("meta", "config.json"))
	return parse(filepath.Join(root.Base(), "meta", "config.json"), data, err)
}

func parse(path string, data []byte, readErr error) Roster {
	roster := Roster{Path: path}
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

// Declarations is the union of the rosters that apply to one delivery
// root: the base roster and the root's own roster. A session roster adds
// handles; it never removes a base declaration.
type Declarations []Roster

// For reads the rosters that apply to deliveryRoot. base is the
// already-resolved base root (from route.Walker.ClassifyRoot or an authenticated pin,
// never a display fallback); "" or base == deliveryRoot means the root is its
// own base.
func For(deliveryRoot, base string) Declarations {
	decls := Declarations{}
	if base != "" && filepath.Clean(base) != filepath.Clean(deliveryRoot) {
		decls = append(decls, Read(base))
	}
	return append(decls, Read(deliveryRoot))
}

// Declares reports whether any roster declares handle; the reserved human
// handle is always declared.
func (d Declarations) Declares(handle string) bool {
	if handle == thread.HumanHandle {
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

// Err joins the failures of rosters that exist but cannot be read, so a
// strict caller can refuse instead of judging from a partial union.
func (d Declarations) Err() error {
	var errs []error
	for _, roster := range d {
		if roster.Err != nil {
			errs = append(errs, roster.Err)
		}
	}
	return errors.Join(errs...)
}

// AnyPresent reports whether at least one roster exists. A queue with no
// roster at all declares nothing, so send and reply do not judge its handles.
func (d Declarations) AnyPresent() bool {
	for _, roster := range d {
		if roster.Present {
			return true
		}
	}
	return false
}

// Registration is the classifier's verdict for one (delivery root,
// handle). Identity is set whenever a valid record exists, also for a
// declared handle. Diagnostics holds what kept the verdict from being
// complete: an unreadable roster when the handle is not otherwise declared,
// or an identity record that exists but is invalid.
type Registration struct {
	Handle      string
	Status      Status
	Kind        string
	Identity    *agentidentity.Record
	Diagnostics []error
}

// Classify is the one registration classifier. identityErr
// is the result of reading the handle's identity record in the same delivery
// root: nil with rec, a not-exist error for "no record", or any other error
// for a record that cannot be trusted.
func Classify(decls Declarations, handle string, rec agentidentity.Record, identityErr error) Registration {
	reg := Registration{Handle: handle, Kind: "agent", Status: Unregistered}
	switch {
	case identityErr == nil:
		reg.Identity = &rec
	case !errors.Is(identityErr, os.ErrNotExist):
		reg.Diagnostics = append(reg.Diagnostics, identityErr)
	}
	switch {
	case handle == thread.HumanHandle:
		reg.Kind = "human"
		reg.Status = Declared
	case decls.Declares(handle):
		reg.Status = Declared
	default:
		if reg.Identity != nil {
			reg.Status = Observed
		}
		if err := decls.Err(); err != nil {
			reg.Diagnostics = append(reg.Diagnostics, err)
		}
	}
	return reg
}

// ClassifyAt classifies handle in deliveryRoot, reading its identity
// record from that root's mailbox.
func ClassifyAt(decls Declarations, deliveryRoot, handle string) Registration {
	rec, err := agentidentity.Read(deliveryRoot, handle)
	return Classify(decls, handle, rec, err)
}

// RegistrationJSON is the additive projection every agent listing
// embeds. It never reuses an existing field: kind, status and last_seen keep
// their meanings, and a declared handle still shows its observed identity.
type RegistrationJSON struct {
	RegistrationStatus      Status        `json:"registration_status"`
	Identity                *IdentityJSON `json:"identity,omitempty"`
	RegistrationDiagnostics []string      `json:"registration_diagnostics,omitempty"`
}

// IdentityJSON is the observed identity in RegistrationJSON.
type IdentityJSON struct {
	Harness    string `json:"harness"`
	Wrapper    string `json:"wrapper,omitempty"`
	ObservedAt string `json:"observed_at"`
}

// JSON is the additive projection of r.
func (r Registration) JSON() RegistrationJSON {
	out := RegistrationJSON{RegistrationStatus: r.Status}
	if r.Identity != nil {
		out.Identity = &IdentityJSON{
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

// AgentRegistration classifies handle in the delivery root root against the
// rosters of root and of its base as w classifies it, and the identity record
// in root. It is discovery only: callers must not treat any status as
// authority to deliver, repair or attach.
func AgentRegistration(w route.Walker, root, handle string) (status string, diagnostics []string) {
	reg := ClassifyAt(For(root, w.ClassifyRoot(root)), root, handle).JSON()
	return string(reg.RegistrationStatus), reg.RegistrationDiagnostics
}
