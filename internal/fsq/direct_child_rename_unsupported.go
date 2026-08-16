//go:build !darwin && !linux && !windows

package fsq

import (
	"errors"
	"fmt"
)

func (r *DeliveryRoot) renameDirectChildNoReplace(_, _ string) error {
	return fmt.Errorf("exclusive direct-child publication: %w", errors.ErrUnsupported)
}
