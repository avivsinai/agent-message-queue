//go:build windows

package launch

import "os"

func validateExactAMQRCFileInfo(os.FileInfo) error { return nil }
