//go:build darwin || linux

package wakemutation

import "fmt"

type LinkAtFunc func(fromDirFD int, from string, toDirFD int, to string, flags int) error

func (lease *Lease) LinkAt(
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
	flags int,
) error {
	return lease.LinkAtWith(unixLinkAt, fromDirFD, from, toDirFD, to, flags)
}

func (lease *Lease) LinkAtWith(
	link LinkAtFunc,
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
	flags int,
) error {
	if link == nil {
		return fmt.Errorf("wake link capability is missing")
	}
	return lease.withEffect(func() error {
		return link(fromDirFD, from, toDirFD, to, flags)
	})
}
