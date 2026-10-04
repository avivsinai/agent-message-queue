package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/sharestate"
)

func init() { claude.RegisterAllowFactory(allowConfig) }

// allowConfig is what an allow from Buzz needs for one hook pin (bead
// 611.42.4): the share serving a Claude session, read from the pinned
// root's manifest and enrolled credentials, and a verifier that checks the
// owner's signed ✅ against that share and then reads the approval
// message's edits and deletions from the share's relay, signed in with the
// share's body key as serve is. A pin without an owner and a root allows
// nothing.
func allowConfig(pin claude.HookPin) claude.AllowConfig {
	cfg := claude.AllowConfig{Owner: pin.Owner}
	if !claude.ValidOwner(pin.Owner) || !filepath.IsAbs(pin.Root) {
		return cfg
	}
	cfg.Share = func(sessionID string) (claude.AllowShare, error) {
		sh, url, err := pinnedShare(pin, sessionID)
		if err != nil {
			return claude.AllowShare{}, err
		}
		creds, err := sharestate.Load(pin.Root, sh.Session)
		if err != nil {
			return claude.AllowShare{}, fmt.Errorf("share %s credentials: %w", sh.Session, err)
		}
		if creds.Owner != pin.Owner {
			return claude.AllowShare{}, fmt.Errorf("share %s is enrolled to another owner", sh.Session)
		}
		return claude.AllowShare{Owner: sh.OwnerPubKey, Body: creds.Body.PublicKeyHex(), Channel: sh.DMChannelID, Target: sh.Target,
			Session: sh.Session, RelayURL: url}, nil
	}
	cfg.Verify = func(ctx context.Context, evidence json.RawMessage, want claude.AllowCheck) error {
		check := buzzio.ApproveCheck{Owner: want.Share.Owner, Body: want.Share.Body, Channel: want.Share.Channel, Target: want.Share.Target,
			Prompt: want.Prompt, NotBefore: want.NotBefore, NotAfter: want.NotAfter}
		msg, err := buzzio.VerifyApproveEvidence(evidence, check)
		if err != nil {
			return err
		}
		rc, err := relayConfigFor(pin.Root, want.Share.RelayURL, manifest.Share{Session: want.Share.Session, OwnerPubKey: want.Share.Owner})()
		if err != nil {
			return fmt.Errorf("relay sign-in: %w", err)
		}
		conn, err := relay.Connect(ctx, rc)
		if err != nil {
			return fmt.Errorf("relay: %w", err)
		}
		defer conn.Close()
		return historyError(buzzio.CheckHistory(ctx, conn, msg, check))
	}
	return cfg
}

// historyError marks an altered approval message for the hook and the
// attachment.
func historyError(err error) error {
	if errors.Is(err, buzzio.ErrAltered) {
		return fmt.Errorf("%w: %v", claude.ErrAllowAltered, err)
	}
	return err
}

// pinnedShare is the relay share of the pinned root's manifest that serves
// sessionID: the pinned session when the pin names one, else the share
// whose native session is sessionID. It must be owned by the pinned owner,
// name a DM channel, and serve exactly that Claude session.
func pinnedShare(pin claude.HookPin, sessionID string) (manifest.Share, string, error) {
	path := manifest.DefaultPath(filepath.Join(pin.Root, stateDirName))
	mf, err := manifest.Load(path)
	if err != nil {
		return manifest.Share{}, "", fmt.Errorf("manifest %s: %w", path, err)
	}
	if mf.Relay == nil {
		return manifest.Share{}, "", fmt.Errorf("manifest %s has no relay", path)
	}
	var found []manifest.Share
	for _, sh := range mf.Relay.Shares {
		if pin.Session != "" && sh.Session != pin.Session || sh.NativeSessionID != sessionID {
			continue
		}
		found = append(found, sh)
	}
	switch {
	case len(found) != 1:
		return manifest.Share{}, "", fmt.Errorf("manifest %s has %d share(s) for this Claude session", path, len(found))
	case found[0].OwnerPubKey != pin.Owner:
		return manifest.Share{}, "", fmt.Errorf("share %s is not owned by the pinned owner", found[0].Session)
	case found[0].DMChannelID == "":
		return manifest.Share{}, "", fmt.Errorf("share %s has no DM channel", found[0].Session)
	}
	return found[0], mf.Relay.URL, nil
}

// installApprovalHook writes the PermissionRequest hook, pinning the owner
// given by --owner (hex or npub) or, without it, the one owner of the
// manifest's relay shares. With no owner found the hook is reject-only.
func installApprovalHook(home string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-approval-hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ownerFlag := fs.String("owner", "", "owner public key (64 hex or npub) whose signed ✅ in Buzz can allow a tool call (default: the relay shares' owner in the manifest)")
	root := fs.String("root", os.Getenv("AM_ROOT"), "AMQ root whose manifest and enrolled shares the hook reads to verify an allow (default AM_ROOT)")
	session := fs.String("session", "", "pin one share session (default: the share whose native session is the Claude session)")
	manifestPath := fs.String("manifest", "", "path to the adapter manifest (default: <root>/"+stateDirName+"/manifest.json)")
	if fs.Parse(args) != nil {
		return 2
	}
	owner, err := approvalOwner(*ownerFlag, *root, *manifestPath)
	if err != nil {
		say(stderr, "install-approval-hook: %v\n", err)
		return 1
	}
	pin := claude.HookPin{Owner: owner}
	if owner != "" {
		if pin.Root, err = filepath.Abs(*root); err != nil || *root == "" {
			say(stderr, "install-approval-hook: an owner pin needs the AMQ root: pass --root or set AM_ROOT\n")
			return 1
		}
		pin.Session = *session
	}
	bin, err := os.Executable()
	if err != nil {
		bin = "amq-remote"
	}
	if err := claude.InstallPermissionHook(home, bin, claude.DefaultPermissionWait, pin); err != nil {
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
