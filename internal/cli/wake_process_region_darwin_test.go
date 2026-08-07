//go:build darwin

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

func TestDarwinProcessRegionABILayout(t *testing.T) {
	if got := unsafe.Sizeof(darwinProcRegionWithPathInfo{}); got != darwinProcRegionWithPathInfoSize {
		t.Fatalf("proc_regionwithpathinfo size = %d, want %d", got, darwinProcRegionWithPathInfoSize)
	}
}

func TestDarwinMappedImageMatchesCurrentProcessVnode(t *testing.T) {
	got, err := inspectDarwinWakeMappedImage(os.Getpid())
	if err != nil {
		t.Fatalf("inspect mapped current image: %v", err)
	}
	path, err := readDarwinProcessExecutablePath(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := captureWakeFileIdentity(info)
	if !ok {
		t.Fatal("capture current process identity")
	}
	if got.Path != path || got.Identity != want || got.Size != info.Size() {
		t.Fatalf("mapped image = %#v, want path=%q identity=%#v size=%d", got, path, want, info.Size())
	}
}

func TestDarwinMappedImageDeduplicatesExecutableRegionsForOneVnode(t *testing.T) {
	path := "/tmp/amq-mapped-test"
	identity := wakeFileIdentity{Device: 17, Inode: 41, CTimeSec: 7, CTimeNsec: 9}
	regions := []darwinProcRegionWithPathInfo{
		testDarwinMappedRegion(0x1000, 0x1000, path, identity, 8192),
		testDarwinMappedRegion(0x2000, 0x1000, path, identity, 8192),
	}
	var calls int
	stubDarwinProcessRegionReader(t, func(_ int, address uint64) (darwinProcRegionWithPathInfo, bool, error) {
		wantAddresses := []uint64{0, 0x2000, 0x3000}
		if calls >= len(wantAddresses) || address != wantAddresses[calls] {
			t.Fatalf("query %d address = %#x, want %#x", calls, address, wantAddresses[calls])
		}
		if calls == len(regions) {
			calls++
			return darwinProcRegionWithPathInfo{}, false, nil
		}
		region := regions[calls]
		calls++
		return region, true, nil
	})

	got, err := scanDarwinWakeMappedImage(42, path)
	if err != nil {
		t.Fatalf("scan mapped image: %v", err)
	}
	if got != (darwinMappedWakeImage{Path: path, Identity: identity, Size: 8192}) {
		t.Fatalf("mapped image = %#v", got)
	}
}

func TestDarwinMappedImageRejectsTwoExecutableVnodesAtExactPath(t *testing.T) {
	path := "/tmp/amq-mapped-test"
	regions := []darwinProcRegionWithPathInfo{
		testDarwinMappedRegion(0x1000, 0x1000, path, wakeFileIdentity{Device: 17, Inode: 41}, 8192),
		testDarwinMappedRegion(0x2000, 0x1000, path, wakeFileIdentity{Device: 17, Inode: 42}, 8192),
	}
	var calls int
	stubDarwinProcessRegionReader(t, func(int, uint64) (darwinProcRegionWithPathInfo, bool, error) {
		if calls == len(regions) {
			return darwinProcRegionWithPathInfo{}, false, nil
		}
		region := regions[calls]
		calls++
		return region, true, nil
	})

	if _, err := scanDarwinWakeMappedImage(42, path); err == nil || !strings.Contains(err.Error(), "found 2 candidates") {
		t.Fatalf("scan error = %v, want ambiguous mapped vnodes", err)
	}
}

func TestDarwinMappedImageRejectsNonAdvancingAndMalformedRegions(t *testing.T) {
	path := "/tmp/amq-mapped-test"
	tests := []struct {
		name   string
		region darwinProcRegionWithPathInfo
		want   string
	}{
		{
			name:   "zero size",
			region: testDarwinMappedRegion(0x1000, 0, path, wakeFileIdentity{Device: 17, Inode: 41}, 8192),
			want:   "did not advance",
		},
		{
			name: "unterminated path",
			region: func() darwinProcRegionWithPathInfo {
				region := testDarwinMappedRegion(0x1000, 0x1000, path, wakeFileIdentity{Device: 17, Inode: 41}, 8192)
				for index := range region.Vnode.Path {
					region.Vnode.Path[index] = 'x'
				}
				return region
			}(),
			want: "unterminated path",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubDarwinProcessRegionReader(t, func(int, uint64) (darwinProcRegionWithPathInfo, bool, error) {
				return tc.region, true, nil
			})
			if _, err := scanDarwinWakeMappedImage(42, path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("scan error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDarwinMappedImageKeepsQueryFailureUnknown(t *testing.T) {
	stubDarwinProcessRegionReader(t, func(int, uint64) (darwinProcRegionWithPathInfo, bool, error) {
		return darwinProcRegionWithPathInfo{}, false, errors.New("query denied")
	})
	if _, err := scanDarwinWakeMappedImage(42, "/tmp/amq-mapped-test"); err == nil || !strings.Contains(err.Error(), "query denied") {
		t.Fatalf("scan error = %v, want query failure", err)
	}
}

func TestDarwinMappedImageRejectsChangingSecondScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amq")
	stubDarwinProcessExecutablePath(t, func(int) (string, error) { return path, nil })
	first := testDarwinMappedRegion(0x1000, 0x1000, path, wakeFileIdentity{Device: 17, Inode: 41}, 8192)
	second := testDarwinMappedRegion(0x1000, 0x1000, path, wakeFileIdentity{Device: 17, Inode: 42}, 8192)
	var calls int
	stubDarwinProcessRegionReader(t, func(int, uint64) (darwinProcRegionWithPathInfo, bool, error) {
		defer func() { calls++ }()
		switch calls {
		case 0:
			return first, true, nil
		case 1:
			return darwinProcRegionWithPathInfo{}, false, nil
		case 2:
			return second, true, nil
		default:
			return darwinProcRegionWithPathInfo{}, false, nil
		}
	})
	if _, err := inspectDarwinWakeMappedImage(42); err == nil || !strings.Contains(err.Error(), "changed during inspection") {
		t.Fatalf("mapped-image error = %v, want changing evidence refusal", err)
	}
}

func testDarwinMappedRegion(
	address uint64,
	size uint64,
	path string,
	identity wakeFileIdentity,
	fileSize int64,
) darwinProcRegionWithPathInfo {
	var region darwinProcRegionWithPathInfo
	region.Region.Address = address
	region.Region.Size = size
	region.Region.Protection = darwinVMProtExecute
	region.Vnode.Info.Type = darwinVnodeTypeRegular
	region.Vnode.Info.Stat.Mode = darwinModeRegular | 0o755
	region.Vnode.Info.Stat.Device = uint32(identity.Device)
	region.Vnode.Info.Stat.Inode = identity.Inode
	region.Vnode.Info.Stat.ChangeTime = identity.CTimeSec
	region.Vnode.Info.Stat.ChangeTimeNS = identity.CTimeNsec
	region.Vnode.Info.Stat.Size = fileSize
	copy(region.Vnode.Path[:], path)
	return region
}

func stubDarwinProcessRegionReader(
	t *testing.T,
	reader func(int, uint64) (darwinProcRegionWithPathInfo, bool, error),
) {
	t.Helper()
	old := readDarwinProcessRegionPathInfo
	readDarwinProcessRegionPathInfo = reader
	t.Cleanup(func() { readDarwinProcessRegionPathInfo = old })
}
