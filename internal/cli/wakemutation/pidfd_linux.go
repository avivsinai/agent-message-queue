//go:build linux

package wakemutation

import "golang.org/x/sys/unix"

type PidfdSenderFunc func(pidfd int, signal unix.Signal, info *unix.Siginfo, flags int) error

var pidfdSendSignal PidfdSenderFunc = func(
	pidfd int,
	signal unix.Signal,
	info *unix.Siginfo,
	flags int,
) error {
	return unix.PidfdSendSignal(pidfd, signal, info, flags)
}

func PidfdSendSignal(pidfd int, signal unix.Signal, info *unix.Siginfo, flags int) error {
	return pidfdSendSignal(pidfd, signal, info, flags)
}

func (lease *Lease) SendPidfdSignal(pidfd int, signal unix.Signal) error {
	return lease.SendPidfdSignalWith(pidfdSendSignal, pidfd, signal)
}

func (lease *Lease) SendPidfdSignalWith(
	send PidfdSenderFunc,
	pidfd int,
	signal unix.Signal,
) error {
	if send == nil {
		return ErrClosed
	}
	return lease.withEffect(func() error {
		return send(pidfd, signal, nil, 0)
	})
}
