package buzzop

import "fiatjaf.com/nostr"

// Inbound is one owner message for the machine agent's engine. The caller
// has verified Event's id and signature, that it is the plaintext message
// of Channel (kind 9 with one h tag equal to Channel), and that its author
// is the agent's owner. A frontend that cannot establish all of this does
// not build an Inbound and does not call the engine.
type Inbound struct {
	Channel string
	Event   nostr.Event
}
