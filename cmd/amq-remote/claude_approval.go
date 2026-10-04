package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
)

// approvalVerifier is what the PermissionRequest hook accepts as proof of an
// allow: the pinned owner's signed ✅ on the Buzz approval message that
// shows the call (bead 611.42.4).
var approvalVerifier claude.AllowVerifier = buzzio.VerifyApproveEvidence

// installApprovalHook writes the PermissionRequest hook, pinning the owner
// given by --owner (hex or npub) or, without it, the one owner of the
// manifest's relay shares. With no owner found the hook is reject-only.
func installApprovalHook(home string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-approval-hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ownerFlag := fs.String("owner", "", "owner public key (64 hex or npub) whose signed ✅ in Buzz can allow a tool call (default: the relay shares' owner in the manifest)")
	root := fs.String("root", os.Getenv("AM_ROOT"), "AMQ root directory whose manifest names the share owner (default AM_ROOT)")
	manifestPath := fs.String("manifest", "", "path to the adapter manifest (default: <root>/"+stateDirName+"/manifest.json)")
	if fs.Parse(args) != nil {
		return 2
	}
	owner, err := approvalOwner(*ownerFlag, *root, *manifestPath)
	if err != nil {
		say(stderr, "install-approval-hook: %v\n", err)
		return 1
	}
	bin, err := os.Executable()
	if err != nil {
		bin = "amq-remote"
	}
	if err := claude.InstallPermissionHook(home, bin, claude.DefaultPermissionWait, owner); err != nil {
		say(stderr, "install-approval-hook: %v\n", err)
		return 1
	}
	if owner == "" {
		say(stdout, "approval hook installed without an owner pin: Buzz can block a Claude tool call; allow it in the terminal\n")
		return 0
	}
	say(stdout, "approval hook installed: the owner %s can allow a Claude tool call with ✅ in Buzz, or block it with ❌\n", owner)
	say(stdout, "the pin holds only while Claude cannot edit ~/.claude/settings.json or run any Bash command without a prompt: never allow edits to .claude for a session, bypassPermissions mode, or a Bash(*) allow rule\n")
	return 0
}

// approvalOwner is the owner pubkey to pin, as 64 lowercase hex: flag as
// hex or npub, else the one owner of the manifest's relay shares, else "".
func approvalOwner(flagValue, root, manifestPath string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		if strings.HasPrefix(v, "npub1") {
			prefix, val, err := nip19.Decode(v)
			pk, ok := val.(nostr.PubKey)
			if err != nil || prefix != "npub" || !ok {
				return "", fmt.Errorf("--owner %q is not a valid npub", v)
			}
			return pk.Hex(), nil
		}
		if v = strings.ToLower(v); !claude.ValidOwner(v) {
			return "", fmt.Errorf("--owner %q is not 64 hex characters or an npub", flagValue)
		}
		return v, nil
	}
	if manifestPath == "" {
		if root == "" {
			return "", nil
		}
		manifestPath = manifest.DefaultPath(filepath.Join(root, stateDirName))
	}
	mf, err := manifest.Load(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("manifest %s: %w", manifestPath, err)
	}
	owner := ""
	if mf.Relay != nil {
		for _, sh := range mf.Relay.Shares {
			switch {
			case sh.OwnerPubKey == "" || sh.OwnerPubKey == owner:
			case owner == "":
				owner = sh.OwnerPubKey
			default:
				return "", fmt.Errorf("manifest %s shares name more than one owner; pass --owner", manifestPath)
			}
		}
	}
	if owner != "" && !claude.ValidOwner(owner) {
		return "", fmt.Errorf("manifest %s owner %q is not 64 lowercase hex", manifestPath, owner)
	}
	return owner, nil
}
