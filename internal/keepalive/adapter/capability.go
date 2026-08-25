package adapter

// Capability models the wake capability vector from
// docs/adr-wake-capability-vector.md. Each seat advertises the strongest
// delivery it can honestly claim; callers request a minimum. Weaker is
// refused, never substituted (ADR §"refusal over substitution").
type Capability struct {
	Activation    Activation
	Delivery      Delivery
	Session       SessionScope
	RequiresHuman bool
}

// Activation is how strongly a seat can bring its surface to the user.
type Activation int // none < launch < foreground

const (
	ActivationNone Activation = iota
	ActivationLaunch
	ActivationForeground
)

// Delivery is how much of the prompt reaches the composer.
type Delivery int // none < prefilled < submitted

const (
	DeliveryNone Delivery = iota
	DeliveryPrefilled
	DeliverySubmitted
)

// SessionScope is how precisely the seat addresses an existing surface.
type SessionScope int // none < new < existing-exact

const (
	SessionNone SessionScope = iota
	SessionNew
	SessionExistingExact
)

// Satisfies reports whether this seat is at least as strong as the caller's
// minimum on every axis. Weaker is REFUSED, never substituted
// (ADR §"refusal over substitution"). A caller that needs an unattended wake
// (min.RequiresHuman == false) must refuse a requires-human seat; a caller
// that accepts a human handoff (min.RequiresHuman == true) accepts either.
func (c Capability) Satisfies(min Capability) bool {
	if c.Activation < min.Activation {
		return false
	}
	if c.Delivery < min.Delivery {
		return false
	}
	if c.Session < min.Session {
		return false
	}
	if !min.RequiresHuman && c.RequiresHuman {
		return false
	}
	return true
}

// UnknownCapability is the vector assumed for an adapter that does not
// implement CapabilityDeclarer: weakest on every ordered axis
// (Activation/Delivery/Session all None) and most-restrictive on the tolerance
// axis (RequiresHuman true). An undeclared adapter is therefore refused
// unless the caller explicitly tolerates a human-required seat, so an adapter
// that forgets to declare a Capability can never masquerade as an unattended
// full-strength seat. This is worst-case on the tolerance axis, not zero-value:
// the zero-value Capability has RequiresHuman=false, which would let an unknown
// adapter pass the default zero minimum as if it delivered unattended.
func UnknownCapability() Capability { return Capability{RequiresHuman: true} }
