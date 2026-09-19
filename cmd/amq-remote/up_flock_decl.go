package main

import "errors"

// errLifetimeHeld is returned by flockLifetime when another up process
// already owns the companion slot. Cross-platform so tests compile under
// GOOS=windows vet (the flock implementations are per-platform).
var errLifetimeHeld = errors.New("lifetime lock already held by another up process")
