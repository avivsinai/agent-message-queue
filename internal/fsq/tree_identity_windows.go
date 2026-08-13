//go:build windows

package fsq

import (
	"fmt"
	"os"
)

func platformStableTreeIdentity(_ string, _ os.FileInfo) (string, error) {
	return "", fmt.Errorf("Windows identity pinning is out of scope")
}
