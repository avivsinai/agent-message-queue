package cli

import (
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func snapshotMailboxDeliveryRoot(root string, routed, ignorePin bool) (fsq.DeliveryRootIdentity, error) {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return fsq.DeliveryRootIdentity{}, err
	}
	if routed || ignorePin {
		return identity, nil
	}
	pin, err := loadSessionPin()
	if err != nil {
		return fsq.DeliveryRootIdentity{}, err
	}
	if pin.IdentityPin && verifyTreeIdentityInfo(identity.FileInfo(), pin.RootID) != TreeRelationSame {
		return fsq.DeliveryRootIdentity{}, ContextMismatchError("authorized mailbox root identity changed before capability open")
	}
	return identity, nil
}

// deliveryRootIdentityToken is the identity token of the directory an open
// capability holds, or "" when it cannot be read; "" matches no directory.
func deliveryRootIdentityToken(root *fsq.DeliveryRoot) string {
	token, err := platformTreeIdentityToken("", root.FileInfo())
	if err != nil {
		return ""
	}
	return token
}
