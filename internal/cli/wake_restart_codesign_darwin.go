//go:build darwin

package cli

import (
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

var verifyWakeRestartBoundImage = verifyWakeRestartBoundImageDefault

func verifyWakeRestartBoundImagePlatform(image *wakeRestartBoundImage) error {
	return verifyWakeRestartBoundImage(image)
}

func verifyWakeRestartBoundImageDefault(image *wakeRestartBoundImage) error {
	if image == nil || image.file == nil || image.executionPath == "" {
		return fmt.Errorf("bound Darwin wake restart image is missing")
	}
	if err := revalidateBoundWakeRestartImagePlatform(image); err != nil {
		return err
	}
	return selfupgrade.VerifyDarwinCodeSignature(image.executionPath)
}
