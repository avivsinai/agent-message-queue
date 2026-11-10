package selfupgrade

import (
	"fmt"
	"os"
)

// ValidatePathOwnership refuses a file that group or others can write or that
// another user owns.
func ValidatePathOwnership(label, path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s %s is group/world-writable", label, path)
	}
	ownerUID, ownerOK := FileOwnerUID(info)
	currentUID, currentOK := CurrentUID()
	if ownerOK && currentOK && ownerUID != currentUID {
		return fmt.Errorf("%s %s is owned by uid %d, want current uid %d", label, path, ownerUID, currentUID)
	}
	return nil
}
