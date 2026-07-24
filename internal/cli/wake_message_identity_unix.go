//go:build darwin || linux

package cli

// wakeFileIdentity is the stable portion of a mailbox file instance used by
// local baseline filtering.
type wakeFileIdentity struct {
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	CTimeSec  int64  `json:"ctime_sec"`
	CTimeNsec int64  `json:"ctime_nsec"`
}
