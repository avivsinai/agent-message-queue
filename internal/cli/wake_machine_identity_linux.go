//go:build linux

package cli

import (
	"os"
	"strings"
)

// readWakeMachineIDPlatform returns the machine id, the standard stable
// machine identity on Linux.
func readWakeMachineIDPlatform() string {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(data)); isLinuxMachineID(id) {
			return id
		}
	}
	return ""
}

func isLinuxMachineID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
