package adapter

import "testing"

func TestCapabilitySatisfies(t *testing.T) {
	// The honest claude-desktop seat from the live smoke: prefilled + new +
	// requires_human. Activation is launch (deep-link opens the app/surface).
	claudeDesktop := Capability{
		Activation:    ActivationLaunch,
		Delivery:      DeliveryPrefilled,
		Session:       SessionNew,
		RequiresHuman: true,
	}
	// A full-strength TTY seat: foreground + submitted + existing-exact,
	// unattended.
	tty := Capability{
		Activation:    ActivationForeground,
		Delivery:      DeliverySubmitted,
		Session:       SessionExistingExact,
		RequiresHuman: false,
	}
	zero := Capability{}

	tests := []struct {
		name string
		seat Capability
		min  Capability
		want bool
	}{
		{
			name: "submitted-caller vs prefilled seat is refused (the core submit-to-prefill downgrade)",
			seat: claudeDesktop,
			min:  Capability{Delivery: DeliverySubmitted},
			want: false,
		},
		{
			name: "existing-exact-caller vs new-only seat is refused",
			seat: claudeDesktop,
			min:  Capability{Session: SessionExistingExact},
			want: false,
		},
		{
			name: "unattended caller vs requires-human seat is refused",
			seat: claudeDesktop,
			min:  Capability{RequiresHuman: false, Delivery: DeliveryPrefilled, Session: SessionNew},
			want: false,
		},
		{
			name: "human-handoff caller accepts the requires-human prefilled seat",
			seat: claudeDesktop,
			min:  Capability{Delivery: DeliveryPrefilled, Session: SessionNew, RequiresHuman: true},
			want: true,
		},
		{
			name: "equal on all axes satisfies",
			seat: claudeDesktop,
			min:  claudeDesktop,
			want: true,
		},
		{
			name: "stronger-than-min on every axis satisfies",
			seat: tty,
			min:  claudeDesktop,
			want: true,
		},
		{
			name: "zero-value seat satisfies only the zero-value min",
			seat: zero,
			min:  zero,
			want: true,
		},
		{
			name: "zero-value seat does not satisfy a submitted min",
			seat: zero,
			min:  Capability{Delivery: DeliverySubmitted},
			want: false,
		},
		{
			name: "requires-human seat with unattended min and zero axes is refused",
			seat: Capability{RequiresHuman: true},
			min:  Capability{RequiresHuman: false},
			want: false,
		},
		{
			name: "requires-human seat with unattended min and otherwise-equal axes is refused",
			seat: Capability{Activation: ActivationLaunch, Delivery: DeliveryPrefilled, Session: SessionNew, RequiresHuman: true},
			min:  Capability{Activation: ActivationLaunch, Delivery: DeliveryPrefilled, Session: SessionNew, RequiresHuman: false},
			want: false,
		},
		{
			name: "requires-human seat with requires-human min and zero axes satisfies",
			seat: Capability{RequiresHuman: true},
			min:  Capability{RequiresHuman: true},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.seat.Satisfies(tt.min); got != tt.want {
				t.Fatalf("Satisfies() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCapabilityZeroValueIsWeakest(t *testing.T) {
	// The zero value must be the weakest seat on the ordered axes: it
	// satisfies nothing stronger than the zero-value minimum on activation,
	// delivery, or session. (RequiresHuman on the min is a tolerance, not a
	// strength axis: a min that accepts human handoff also accepts an
	// unattended seat, so it is deliberately not in this list.)
	zero := Capability{}
	for _, min := range []Capability{
		{Activation: ActivationLaunch},
		{Delivery: DeliveryPrefilled},
		{Session: SessionNew},
	} {
		if zero.Satisfies(min) {
			t.Fatalf("zero-value seat satisfied %+v; want refusal", min)
		}
	}
}
