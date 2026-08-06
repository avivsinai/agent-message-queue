//go:build darwin

package cli

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinProcPIDRegionPathInfo = 8
	darwinVMProtExecute         = 0x04
	darwinVnodeTypeRegular      = 1
	darwinModeTypeMask          = 0o170000
	darwinModeRegular           = 0o100000
	darwinMaxProcessRegions     = 1 << 16

	// struct proc_regionwithpathinfo is composed entirely of fixed-width
	// public proc_info fields and has this layout on both Darwin arm64 and
	// amd64. Keep the assertion adjacent to the Go transcription below.
	darwinProcRegionWithPathInfoSize = 1272
)

type darwinProcRegionInfo struct {
	Protection            uint32
	MaxProtection         uint32
	Inheritance           uint32
	Flags                 uint32
	Offset                uint64
	Behavior              uint32
	UserWiredCount        uint32
	UserTag               uint32
	PagesResident         uint32
	PagesSharedNowPrivate uint32
	PagesSwappedOut       uint32
	PagesDirtied          uint32
	RefCount              uint32
	ShadowDepth           uint32
	ShareMode             uint32
	PrivatePagesResident  uint32
	SharedPagesResident   uint32
	ObjectID              uint32
	Depth                 uint32
	Address               uint64
	Size                  uint64
}

type darwinVInfoStat struct {
	Device       uint32
	Mode         uint16
	LinkCount    uint16
	Inode        uint64
	UID          uint32
	GID          uint32
	AccessTime   int64
	AccessTimeNS int64
	ModifyTime   int64
	ModifyTimeNS int64
	ChangeTime   int64
	ChangeTimeNS int64
	BirthTime    int64
	BirthTimeNS  int64
	Size         int64
	Blocks       int64
	BlockSize    int32
	Flags        uint32
	Generation   uint32
	RDevice      uint32
	Reserved     [2]int64
}

type darwinVnodeInfo struct {
	Stat darwinVInfoStat
	Type int32
	Pad  int32
	FSID [2]int32
}

type darwinVnodeInfoPath struct {
	Info darwinVnodeInfo
	Path [unix.PathMax]byte
}

type darwinProcRegionWithPathInfo struct {
	Region darwinProcRegionInfo
	Vnode  darwinVnodeInfoPath
}

const darwinProcRegionWithPathInfoGoSize = int(unsafe.Sizeof(darwinProcRegionWithPathInfo{}))

var (
	_ [darwinProcRegionWithPathInfoSize - darwinProcRegionWithPathInfoGoSize]byte
	_ [darwinProcRegionWithPathInfoGoSize - darwinProcRegionWithPathInfoSize]byte
)

type darwinMappedWakeImage struct {
	Path     string
	Identity wakeFileIdentity
	Size     int64
}

var readDarwinProcessRegionPathInfo = readDarwinProcessRegionPathInfoDefault

func inspectDarwinWakeMappedImage(pid int) (darwinMappedWakeImage, error) {
	firstPath, err := readDarwinProcessExecutablePath(pid)
	if err != nil {
		return darwinMappedWakeImage{}, fmt.Errorf("resolve wake process executable: %w", err)
	}
	if !filepath.IsAbs(firstPath) || filepath.Clean(firstPath) != firstPath {
		return darwinMappedWakeImage{}, fmt.Errorf("wake process executable path is not canonical and absolute")
	}
	first, err := scanDarwinWakeMappedImage(pid, firstPath)
	if err != nil {
		return darwinMappedWakeImage{Path: firstPath}, err
	}

	secondPath, err := readDarwinProcessExecutablePath(pid)
	if err != nil {
		return darwinMappedWakeImage{Path: firstPath}, fmt.Errorf("recheck wake process executable: %w", err)
	}
	if secondPath != firstPath {
		return darwinMappedWakeImage{}, fmt.Errorf("wake process executable path changed during mapped-image inspection")
	}
	second, err := scanDarwinWakeMappedImage(pid, secondPath)
	if err != nil {
		return darwinMappedWakeImage{Path: secondPath}, err
	}
	if first != second {
		return darwinMappedWakeImage{}, fmt.Errorf("wake mapped executable identity changed during inspection")
	}
	return first, nil
}

func scanDarwinWakeMappedImage(pid int, executablePath string) (darwinMappedWakeImage, error) {
	if pid <= 0 {
		return darwinMappedWakeImage{}, fmt.Errorf("invalid process id %d", pid)
	}
	candidates := make(map[darwinMappedWakeImage]struct{})
	address := uint64(0)
	for scanned := 0; scanned < darwinMaxProcessRegions; scanned++ {
		region, found, err := readDarwinProcessRegionPathInfo(pid, address)
		if err != nil {
			return darwinMappedWakeImage{}, fmt.Errorf("inspect wake process region at %#x: %w", address, err)
		}
		if !found {
			if len(candidates) != 1 {
				return darwinMappedWakeImage{}, fmt.Errorf("wake mapped executable identity is ambiguous: found %d candidates", len(candidates))
			}
			for candidate := range candidates {
				return candidate, nil
			}
		}

		if region.Region.Address < address || region.Region.Size == 0 {
			return darwinMappedWakeImage{}, fmt.Errorf("wake process region did not advance")
		}
		next := region.Region.Address + region.Region.Size
		if next <= region.Region.Address {
			return darwinMappedWakeImage{}, fmt.Errorf("wake process region address overflow")
		}

		if region.Region.Protection&darwinVMProtExecute != 0 {
			pathEnd := bytes.IndexByte(region.Vnode.Path[:], 0)
			if pathEnd < 0 {
				return darwinMappedWakeImage{}, fmt.Errorf("wake process region returned an unterminated path")
			}
			mappedPath := string(region.Vnode.Path[:pathEnd])
			stat := region.Vnode.Info.Stat
			if mappedPath == executablePath &&
				region.Vnode.Info.Type == darwinVnodeTypeRegular &&
				stat.Mode&darwinModeTypeMask == darwinModeRegular {
				candidate := darwinMappedWakeImage{
					Path: executablePath,
					Identity: wakeFileIdentity{
						Device:    uint64(stat.Device),
						Inode:     stat.Inode,
						CTimeSec:  stat.ChangeTime,
						CTimeNsec: stat.ChangeTimeNS,
					},
					Size: stat.Size,
				}
				if candidate.Identity.Device == 0 || candidate.Identity.Inode == 0 || candidate.Size <= 0 {
					return darwinMappedWakeImage{}, fmt.Errorf("wake mapped executable vnode identity is incomplete")
				}
				candidates[candidate] = struct{}{}
			}
		}
		address = next
	}
	return darwinMappedWakeImage{}, fmt.Errorf("wake process region scan exceeded %d regions", darwinMaxProcessRegions)
}

func readDarwinProcessRegionPathInfoDefault(
	pid int,
	address uint64,
) (darwinProcRegionWithPathInfo, bool, error) {
	if pid <= 0 {
		return darwinProcRegionWithPathInfo{}, false, fmt.Errorf("invalid process id %d", pid)
	}
	var region darwinProcRegionWithPathInfo
	read, _, errno := unix.Syscall6(
		darwinSysProcInfo,
		darwinProcInfoCallPIDInfo,
		uintptr(pid),
		darwinProcPIDRegionPathInfo,
		uintptr(address),
		uintptr(unsafe.Pointer(&region)),
		uintptr(darwinProcRegionWithPathInfoGoSize),
	)
	runtime.KeepAlive(&region)
	if errno != 0 {
		// proc_pidinfo reports EINVAL when the requested address is beyond the
		// process's final region. The caller still requires exactly one selected
		// executable vnode, so an early EINVAL cannot produce authority.
		if errno == unix.EINVAL {
			return darwinProcRegionWithPathInfo{}, false, nil
		}
		return darwinProcRegionWithPathInfo{}, false, fmt.Errorf("proc_pidinfo pid %d: %w", pid, errno)
	}
	if read == 0 {
		return darwinProcRegionWithPathInfo{}, false, nil
	}
	if read != uintptr(darwinProcRegionWithPathInfoGoSize) {
		return darwinProcRegionWithPathInfo{}, false, fmt.Errorf(
			"proc_pidinfo pid %d returned %d bytes, want %d",
			pid,
			read,
			darwinProcRegionWithPathInfoGoSize,
		)
	}
	return region, true, nil
}
