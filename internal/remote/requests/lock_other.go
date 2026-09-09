//go:build !unix

package requests

import "github.com/avivsinai/agent-message-queue/internal/remote/protocol"

type ownerLock struct{}

func acquireOwnerLock(path string) (*ownerLock, error) {
	return nil, protocol.Refuse(protocol.CodeUnsupported, "the request store needs a unix file lock; %s is not supported on this platform", path)
}

func (l *ownerLock) release() error { return nil }

func isNoSpace(error) bool { return false }
