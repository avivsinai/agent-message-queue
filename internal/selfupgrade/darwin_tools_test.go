//go:build darwin

package selfupgrade

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type darwinToolTestFileInfo struct {
	mode os.FileMode
	sys  *syscall.Stat_t
}

func (info darwinToolTestFileInfo) Name() string       { return "tool" }
func (info darwinToolTestFileInfo) Size() int64        { return 1 }
func (info darwinToolTestFileInfo) Mode() os.FileMode  { return info.mode }
func (info darwinToolTestFileInfo) ModTime() time.Time { return time.Time{} }
func (info darwinToolTestFileInfo) IsDir() bool        { return false }
func (info darwinToolTestFileInfo) Sys() any           { return info.sys }

func TestVerifyDarwinSystemToolRejectsGroupAndWorldWritableModes(t *testing.T) {
	previous := selfUpgradeDarwinToolLstat
	t.Cleanup(func() { selfUpgradeDarwinToolLstat = previous })
	for _, path := range []string{"/usr/bin/codesign", "/bin/ps"} {
		for _, test := range []struct {
			name string
			mode os.FileMode
			want bool
		}{
			{name: "private executable", mode: 0o755},
			{name: "group writable", mode: 0o775, want: true},
			{name: "world writable", mode: 0o757, want: true},
		} {
			t.Run(filepath.Base(path)+"/"+test.name, func(t *testing.T) {
				selfUpgradeDarwinToolLstat = func(got string) (os.FileInfo, error) {
					if got != path {
						t.Fatalf("lstat path = %q, want %q", got, path)
					}
					return darwinToolTestFileInfo{
						mode: test.mode,
						sys:  &syscall.Stat_t{Uid: 0},
					}, nil
				}
				err := verifyDarwinSystemTool(path)
				if (err != nil) != test.want {
					t.Fatalf("verifyDarwinSystemTool() error = %v, wantErr=%t", err, test.want)
				}
			})
		}
	}
}
