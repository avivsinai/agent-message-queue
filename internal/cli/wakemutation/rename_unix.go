//go:build darwin || linux

package wakemutation

import "fmt"

type RenameAtFunc func(fromDirFD int, from string, toDirFD int, to string) error
type RenameNoReplaceAtFunc func(fromDirFD int, from string, toDirFD int, to string) error

func (lease *Lease) RenameAt(
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
) error {
	return lease.RenameAtWith(unixRenameAt, fromDirFD, from, toDirFD, to)
}

func (lease *Lease) RenameAtWith(
	rename RenameAtFunc,
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
) error {
	if rename == nil {
		return fmt.Errorf("wake rename capability is missing")
	}
	return lease.withEffect(func() error {
		return rename(fromDirFD, from, toDirFD, to)
	})
}

func (lease *Lease) RenameNoReplaceAt(
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
) error {
	return lease.RenameNoReplaceAtWith(unixRenameNoReplaceAt, fromDirFD, from, toDirFD, to)
}

func (lease *Lease) RenameNoReplaceAtWith(
	rename RenameNoReplaceAtFunc,
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
) error {
	if rename == nil {
		return fmt.Errorf("wake no-replace rename capability is missing")
	}
	return lease.withEffect(func() error {
		return rename(fromDirFD, from, toDirFD, to)
	})
}
