package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// WakeOwner is the process `coop exec` captured for handle under root,
// read from the wake lock: the authoritative owner of an owner-bound wake,
// else the resume owner. It is only a candidate. It does not read or
// classify the wake process, its liveness, or its readiness; the caller must
// prove the candidate against the live process.
//
// The lock is trusted as written by the same user, like the remote binding
// files: the reader refuses a lock owned by another user or writable by
// group or others, but nothing proves that `coop exec` wrote the owner.
func WakeOwner(root, handle string) (launch.HolderIdentity, bool) {
	// The lock records no pid namespace. Darwin has none, so the recorded pid
	// names a process in this process's namespace; elsewhere (Linux) it may
	// name a process of another namespace, so the candidate is unproven.
	if runtime.GOOS != "darwin" {
		return launch.HolderIdentity{}, false
	}
	if fsq.ValidateHandle(handle) != nil {
		return launch.HolderIdentity{}, false
	}
	lockPath := filepath.Join(fsq.AgentBase(root, handle), wakeLockFileName)
	inspection := readWakeLockMetadataWithReader(root, handle, lockPath, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileWithInfo(lockPath)
	})
	if !inspection.Exists || inspection.Status != wakeLockMissing {
		return launch.HolderIdentity{}, false
	}
	lock := inspection.Lock
	if validateWakeLockFormat(lock, inspection.fileInfo) != nil || lock.Agent != handle {
		return launch.HolderIdentity{}, false
	}
	if strings.TrimSpace(lock.Root) == "" || !filepath.IsAbs(lock.Root) {
		return launch.HolderIdentity{}, false
	}
	lockRoot, err := canonicalizeWakeRoot(lock.Root)
	if err != nil || lockRoot != inspection.Root {
		return launch.HolderIdentity{}, false
	}
	owner := lock.Owner
	if owner == nil && lock.ResumeSchema == wakeResumeSchemaV2 {
		owner = lock.ResumeOwner
	}
	if owner == nil || validateAuthoritativeWakeOwner(*owner) != nil {
		return launch.HolderIdentity{}, false
	}
	return launch.HolderIdentity{PID: owner.PID, ProcessStart: owner.ProcessStart, BootID: owner.BootID}, true
}
