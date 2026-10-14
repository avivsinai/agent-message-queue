//go:build !darwin && !linux

package xferv3

import (
	"fmt"
	"os"
)

func fullSync(*os.File) error {
	return fmt.Errorf("xferv3: durable sync is unsupported on this OS")
}
