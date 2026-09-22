//go:build windows

package fsq

var packageSyncDirFaultForTest func(dir string) error

func (r *DeliveryRoot) syncDir(dir string) error {
	if r.syncDirForTest != nil {
		return r.syncDirForTest(dir)
	}
	if packageSyncDirFaultForTest != nil {
		return packageSyncDirFaultForTest(dir)
	}
	return r.syncDirPlatform(dir)
}

func (r *DeliveryRoot) syncDirPlatform(_ string) error {
	// Deliberate no-op on Windows (bead u35, architect ruling 2026-09-22 —
	// replaces the 09:18Z implement ruling). A directory flush is NOT part
	// of the Windows durability contract because the platform provides the
	// guarantees by other means: rename durability comes from
	// MOVEFILE_WRITE_THROUGH on MoveFileEx (atomic_replace_windows.go,
	// replaceFile), and file data durability from FlushFileBuffers on the
	// FILE handle (write access, held by writeAndSync). A directory flush
	// would require GENERIC_WRITE on a directory handle obtained through
	// NtCreateFile with a RootDirectory object attribute — nothing in the
	// repo or the stdlib does that, and RocksDB's WinDirectory::Fsync (the
	// cited precedent) returns OK without flushing either. The fault hooks
	// stay honoured so tests still pin the call structure.
	return nil
}
