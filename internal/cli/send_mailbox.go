package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type mailboxConfigSelection struct {
	ConfigFS      *fsq.DeliveryRoot
	Shared        bool
	AuthorityRoot string
	close         func()
}

func (s mailboxConfigSelection) Close() {
	if s.close != nil {
		s.close()
	}
}

// openMailboxConfigSelection is the single config-capability selector for
// delivery commands. When configBase has an active config, authorization comes
// from that retained capability; an absent base config falls back to the
// delivery root. Other base-config errors stay selected so the authorization
// layer reports them instead of silently weakening to the delivery root.
func openMailboxConfigSelection(
	deliveryFS *fsq.DeliveryRoot,
	deliveryRoot, configBase, expectedBaseRootID string,
) (mailboxConfigSelection, error) {
	selection := mailboxConfigSelection{
		ConfigFS: deliveryFS,
		close:    func() {},
	}
	if configBase == "" || filepath.Clean(configBase) == filepath.Clean(deliveryRoot) {
		return selection, nil
	}

	configIdentity, err := fsq.SnapshotDeliveryRoot(configBase)
	if err != nil {
		return mailboxConfigSelection{}, err
	}
	if expectedBaseRootID != "" &&
		verifyTreeIdentityInfo(configIdentity.FileInfo(), expectedBaseRootID) != TreeRelationSame {
		return mailboxConfigSelection{}, ContextMismatchError("authorized base root identity changed before config capability open")
	}
	baseConfigFS, err := fsq.OpenDeliveryRoot(configBase, configIdentity)
	if err != nil {
		return mailboxConfigSelection{}, err
	}
	if _, configErr := baseConfigFS.ReadRegularNoFollow(filepath.Join("meta", "config.json")); os.IsNotExist(configErr) {
		_ = baseConfigFS.Close()
		return selection, nil
	}

	selection.ConfigFS = baseConfigFS
	selection.Shared = true
	selection.AuthorityRoot = configBase
	selection.close = func() { _ = baseConfigFS.Close() }
	return selection, nil
}

// localMailboxConfigAuthority resolves configuration authority for a local
// delivery root. A respected named-session pin is strongest. A respected
// sessionless pin deliberately keeps its exact root as authority. Without a
// respected pin, a classified session inherits its base config; the selector
// still falls back to the session-local config when the base has none.
func localMailboxConfigAuthority(
	deliveryRoot string,
	pin sessionPin,
	ignoreSessionPin bool,
) (configBase, expectedBaseRootID string) {
	if pin.Present && !ignoreSessionPin {
		if pin.Session == "" {
			return "", ""
		}
		if pin.IdentityPin {
			return pin.BaseRoot, pin.BaseRootID
		}
		return pin.BaseRoot, ""
	}
	return classifyRoot(deliveryRoot), ""
}

// openLocalMailboxAuthorization selects the same local config authority used
// by send. allowMissingConfig is reserved for reply's legacy support for
// already-complete unconfigured mailboxes; route parity with send must pass
// false and fail as uninitialized when no active config exists.
func openLocalMailboxAuthorization(
	deliveryFS *fsq.DeliveryRoot,
	deliveryRoot string,
	pin sessionPin,
	ignoreSessionPin bool,
	allowMissingConfig bool,
) (*fsq.MailboxConfigAuthorization, func(), error) {
	configBase, expectedBaseRootID := localMailboxConfigAuthority(
		deliveryRoot,
		pin,
		ignoreSessionPin,
	)
	selection, err := openMailboxConfigSelection(
		deliveryFS,
		deliveryRoot,
		configBase,
		expectedBaseRootID,
	)
	if err != nil {
		return nil, func() {}, err
	}

	authorization, inventory, err := fsq.OpenMailboxConfigAuthorization(selection.ConfigFS)
	if err != nil {
		selection.Close()
		if allowMissingConfig && inventory.ActiveConfigStatus == string(fsq.MailboxPathMissing) {
			return nil, func() {}, nil
		}
		return nil, func() {}, sendMailboxAuthorizationError(deliveryRoot, inventory)
	}
	cleanup := func() {
		_ = authorization.Close()
		selection.Close()
	}
	return authorization, cleanup, nil
}

func validateAuthorizedLocalMailbox(
	root *fsq.DeliveryRoot,
	authorization *fsq.MailboxConfigAuthorization,
	target string,
) error {
	effectiveAgents := withReservedHumanHandle(authorization.ConfiguredAgents())
	// Default sends warn but permit an unconfigured requested handle, and the
	// repair API accepts that same requested handle. Include it in this
	// read-only plan so route/reply validation matches the delivery path;
	// callers still own strict roster refusal before any repair.
	effectiveAgents = append(effectiveAgents, target)
	inventory, err := fsq.InspectMailboxLayoutWithAuthorization(root, authorization, effectiveAgents...)
	if err != nil {
		return err
	}
	for _, mailbox := range inventory.Mailboxes {
		if mailbox.Handle != target {
			continue
		}
		if len(mailbox.Issues) == 0 || mailbox.RepairEligible {
			return nil
		}
		return fmt.Errorf("mailbox for %q is incomplete: %s", target, strings.Join(mailbox.Issues, ","))
	}
	return fmt.Errorf("mailbox for %q is absent from the authorized layout", target)
}

func prepareLocalSendMailboxes(root *fsq.DeliveryRoot, authorization *fsq.MailboxConfigAuthorization, displayRoot string, recipients []string) error {
	result := fsq.RepairMailboxLayoutForAgentsWithAuthorization(root, authorization, recipients)
	if result.Status != "repaired" {
		return sendMailboxRepairError(displayRoot, result)
	}
	return nil
}

func sendMailboxAuthorizationError(displayRoot string, inventory fsq.MailboxInventory) error {
	if inventory.ActiveConfigStatus == string(fsq.MailboxPathMissing) {
		return NotFoundError(
			"AMQ root %s is not initialized (%s); run 'amq init --root %s --agents <agents>' or 'amq coop init --root %s'",
			displayRoot,
			inventory.ActiveConfigIssue,
			displayRoot,
			displayRoot,
		)
	}
	return fmt.Errorf(
		"cannot authorize destination mailbox layout at %s: active config %s (%s)",
		displayRoot,
		inventory.ActiveConfigStatus,
		inventory.ActiveConfigIssue,
	)
}

func sendMailboxRepairError(displayRoot string, result fsq.MailboxRepairResult) error {
	if result.Failure == nil {
		return fmt.Errorf("destination mailbox repair at %s ended with status %s", displayRoot, result.Status)
	}
	return fmt.Errorf(
		"destination mailbox repair at %s failed (%s/%s%s): %s",
		displayRoot,
		result.Failure.Code,
		result.Failure.Stage,
		sendMailboxRepairPath(result.Failure.Path),
		result.Failure.Message,
	)
}

func sendMailboxRepairPath(path string) string {
	if path == "" {
		return ""
	}
	return " " + path
}
