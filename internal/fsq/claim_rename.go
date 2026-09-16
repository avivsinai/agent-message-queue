package fsq

import "fmt"

// ClaimCollisionError reports a claim rename that found the destination name
// already present. Normal AMQ flows never hold the same filename in both
// inbox/new and inbox/cur — a committed claim removes new, and DLQ retry
// refuses redelivery while a retained cur exists — so a collision means
// external reintroduction or an invariant violation. It is a loud, terminal
// condition: the claim is neither won nor lost, and callers must not treat it
// as either.
type ClaimCollisionError struct {
	NewPath string
	CurPath string
}

func (e *ClaimCollisionError) Error() string {
	return fmt.Sprintf(
		"claim collision: %s and %s both exist; refusing to replace the retained copy",
		e.NewPath, e.CurPath,
	)
}

// claimCommittedResidueError reports a claim whose destination name was
// created but whose source name could not be removed. The claim is committed
// — exactly this caller owns the message and must emit it — with source
// residue that a later claimer reconciles. MoveNewToCur translates it into
// the caller-facing CommittedDurabilityError contract.
type claimCommittedResidueError struct {
	Err error
}

func (e *claimCommittedResidueError) Error() string {
	return fmt.Sprintf("claim committed with unresolved source residue: %v", e.Err)
}

func (e *claimCommittedResidueError) Unwrap() error { return e.Err }
