//go:build !darwin && !linux

package selfupgrade

import "os"

func imageFileOwnerUID(os.FileInfo) (int, bool) { return 0, false }

func imageCurrentUID() (int, bool) { return 0, false }
