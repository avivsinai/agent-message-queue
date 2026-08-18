//go:build !unix

package launch

import "os"

func fileInode(os.FileInfo) (uint64, bool) { return 0, false }
