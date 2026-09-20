//go:build !unix

package sender

import (
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// spoolLock is unsupported on non-unix platforms. The spool requires a
// cross-process advisory flock to serialize filesystem transactions; without
// it, concurrent Open instances could corrupt envelope files. Open refuses
// CodeUnsupported before accepting durable intent on these platforms, so a
// spoolLock is never acquired here.
type spoolLock struct{}

func acquireSpoolLock(path string) (*spoolLock, error) {
	return nil, protocol.Refuse(protocol.CodeUnsupported, "the sender spool needs a unix file lock; %s is not supported on this platform", path)
}

func (l *spoolLock) release() error { return nil }

// withLock on non-unix refuses — Open should have already refused, so
// reaching here is a programming error surfaced as CodeUnsupported.
func (s *Spool) withLock(fn func() error) error {
	return protocol.Refuse(protocol.CodeUnsupported, "spool locking is not available on this platform")
}
