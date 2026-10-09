//go:build !unix

package amqhome

import "os"

// Other platforms have no portable file owner; the owner check is skipped.
func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }
