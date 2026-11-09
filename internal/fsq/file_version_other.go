//go:build !darwin && !linux

package fsq

import "time"

// LstatVersion is never stable on this platform: without a kernel ctime a
// version cannot stand for content, so every caller reads the file.
func LstatVersion(string, time.Time) (FileVersion, bool, error) {
	return FileVersion{}, false, nil
}

// StatVersion is never stable on this platform, like LstatVersion.
func StatVersion(string, time.Time) (FileVersion, bool, error) {
	return FileVersion{}, false, nil
}
