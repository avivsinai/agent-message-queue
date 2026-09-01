//go:build darwin || linux

package wakemutation

import (
	"errors"
	"sync"
)

var ErrClosed = errors.New("wake mutation lease is closed")

// ReleaseFunc releases the lifecycle authority that backs a Lease.
type ReleaseFunc func() error

// Lease is a capability for effects that must finish before its authority is
// released. Close prevents new effects, waits for admitted effects, and then
// calls the backing release function.
type Lease struct {
	mu       sync.Mutex
	cond     *sync.Cond
	closing  bool
	closed   bool
	inFlight int
	release  ReleaseFunc
	closeErr error
}

func New(release ReleaseFunc) *Lease {
	lease := &Lease{release: release}
	lease.cond = sync.NewCond(&lease.mu)
	return lease
}

func (lease *Lease) Active() bool {
	if lease == nil {
		return false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return !lease.closing && !lease.closed
}

func (lease *Lease) Close() error {
	if lease == nil {
		return ErrClosed
	}

	lease.mu.Lock()
	if lease.closed {
		err := lease.closeErr
		lease.mu.Unlock()
		return err
	}
	if lease.closing {
		for !lease.closed {
			lease.cond.Wait()
		}
		err := lease.closeErr
		lease.mu.Unlock()
		return err
	}
	lease.closing = true
	for lease.inFlight != 0 {
		lease.cond.Wait()
	}
	release := lease.release
	lease.release = nil
	lease.mu.Unlock()

	var releaseErr error
	if release != nil {
		releaseErr = release()
	}

	lease.mu.Lock()
	lease.closeErr = releaseErr
	lease.closed = true
	lease.cond.Broadcast()
	lease.mu.Unlock()
	return releaseErr
}

func (lease *Lease) withEffect(fn func() error) error {
	if lease == nil {
		return ErrClosed
	}
	lease.mu.Lock()
	if lease.closing || lease.closed {
		lease.mu.Unlock()
		return ErrClosed
	}
	lease.inFlight++
	lease.mu.Unlock()
	defer func() {
		lease.mu.Lock()
		lease.inFlight--
		if lease.inFlight == 0 {
			lease.cond.Broadcast()
		}
		lease.mu.Unlock()
	}()
	return fn()
}
