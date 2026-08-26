//go:build linux

package cli

import "time"

func cursorChatDirectoryBirthTime(string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}
