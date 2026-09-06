//go:build darwin

package cli

import (
	"os"
	"testing"
)

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
