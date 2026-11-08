//go:build darwin

package selfupgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

const darwinCodeSignProbeTimeout = 5 * time.Second

var (
	selfUpgradeCodesignPath  = "/usr/bin/codesign"
	selfUpgradeCodesignProbe = RunBoundedProbe
	selfUpgradeDarwinExec    = syscall.Exec
)

func execSupportedPlatform() bool { return true }

func verifyDarwinCodeSignature(stagePath string) error {
	if err := verifyDarwinSystemTool(selfUpgradeCodesignPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("verify Darwin code signature tool: %w", err)
		}
		return fmt.Errorf(
			"verify Darwin code signature: /usr/bin/codesign must exist and pass validation; otherwise replacement is refused: %w",
			err,
		)
	}
	ctx, cancel := context.WithTimeout(context.Background(), darwinCodeSignProbeTimeout)
	defer cancel()
	out, err := selfUpgradeCodesignProbe(
		ctx,
		selfUpgradeCodesignPath,
		[]string{"--verify", "--strict", stagePath},
		BoundedProbeOptions{Env: os.Environ()},
	)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf(
				"verify Darwin code signature: codesign probe timed out: %w",
				errors.Join(ctx.Err(), err),
			)
		}
		return fmt.Errorf("verify Darwin code signature for %s: %w", stagePath, err)
	}
	if diagnostic := strings.TrimSpace(string(out)); diagnostic != "" {
		return fmt.Errorf("verify Darwin code signature for %s: ambiguous codesign output %q", stagePath, diagnostic)
	}
	return nil
}

func VerifyDarwinCodeSignature(stagePath string) error {
	return verifyDarwinCodeSignature(stagePath)
}

func execImagePlatform(candidate ImageEvidence, argv, env []string) error {
	if candidate.ExecutionPath == "" {
		return errors.New("self-upgrade candidate path is empty")
	}
	stored, lock, err := storeImage(candidate)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	// Darwin cannot exec an open file descriptor. The store entry is a private
	// copy that AMQ never replaces, and its signature was verified just
	// before this pathname exec; same-UID races after that validation are
	// outside AMQ's threat model, as for wake self-upgrade.
	return selfUpgradeDarwinExec(stored.ExecutionPath, argv, env)
}
