package registry

import "errors"

// LifetimeLockPath derives the process-lifetime lock path for a registry
// entry. amq-remote up takes this lock for its whole life (611.13.2 D); a
// row whose lock exists but is NOT held is a phantom - its owner died hard
// (kill -9) without running its deferred Forget - and may be reclaimed by
// the next up and marked stale by doctor --ops.
//
// One derivation lives here because both consumers (up reclaim, doctor
// --ops) must probe the same file. The previous spelling lived in
// cmd/amq-remote and was invisible to the keepalive side.
//
// Platform split (611.13.2 review): the probe itself lives in
// lifetime_lock_unix.go (flock-capable platforms) and lifetime_lock_other.go
// (everything else). On non-flock platforms the probe reports an error so
// callers fail closed - never a silent not-held.
func LifetimeLockPath(regPath, entryID string) string {
	return regPath + ".up-" + entryID + ".lock"
}

// errLockProbeFailed wraps a probe error that is neither held nor free.
var errLockProbeFailed = errors.New("registry: lifetime lock probe failed")
