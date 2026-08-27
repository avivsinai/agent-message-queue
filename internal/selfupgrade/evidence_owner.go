package selfupgrade

import (
	"fmt"
	"os"
)

func validateImagePathOwnership(label, path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s %s is group/world-writable", label, path)
	}
	ownerUID, ownerOK := imageFileOwnerUID(info)
	currentUID, currentOK := imageCurrentUID()
	if ownerOK && currentOK && ownerUID != currentUID {
		return fmt.Errorf("%s %s is owned by uid %d, want current uid %d", label, path, ownerUID, currentUID)
	}
	return nil
}
