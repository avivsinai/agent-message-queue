//go:build darwin || linux

package cli

// wakeFileIdentity is the stable portion of a mailbox file instance used by
// local baseline filtering.
type wakeFileIdentity struct {
	Device    uint64
	Inode     uint64
	CTimeSec  int64
	CTimeNsec int64
}
