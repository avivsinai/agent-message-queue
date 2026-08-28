//go:build darwin

package selfupgrade

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var selfUpgradeDarwinToolLstat = os.Lstat

func verifyDarwinSystemTool(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("darwin system tool path is not absolute and canonical")
	}
	info, err := selfUpgradeDarwinToolLstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || stat.Uid != 0 {
		return fmt.Errorf("darwin system tool %s is not a root-owned executable regular file", path)
	}
	return nil
}
