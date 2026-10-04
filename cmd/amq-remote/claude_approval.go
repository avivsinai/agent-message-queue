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
// 611.42.4): the share the pin names (relay, body, DM channel, target), and
// a verifier that checks the owner's signed ✅ against that share and then
// reads the approval message's edits and deletions from the pinned relay,
// signed in with the share's enrolled body key as serve is. The manifest is
// never read: a rewritten manifest changes nothing an allow must prove.
// The root and session only locate the body secret, whose pubkey must be
// the pinned body. A pin missing any flag allows nothing.
func allowConfig(pin claude.HookPin) claude.AllowConfig {
	cfg := claude.AllowConfig{Owner: pin.Owner}
	if !pin.Complete() {
		return cfg
	}
	cfg.Share = func(string) (claude.AllowShare, error) {
		return claude.AllowShare{Owner: pin.Owner, Body: pin.Body, Channel: pin.Channel, Target: pin.Target,
			Session: pin.Session, RelayURL: pin.Relay}, nil
	}
	cfg.Verify = func(ctx context.Context, evidence json.RawMessage, want claude.AllowCheck) error {
		check := buzzio.ApproveCheck{Owner: pin.Owner, Body: pin.Body, Channel: pin.Channel, Target: pin.Target,
			Prompt: want.Prompt, NotBefore: want.NotBefore, NotAfter: want.NotAfter}
		msg, err := buzzio.VerifyApproveEvidence(evidence, check)
		if err != nil {
			return err
		}
		rc, err := relayConfigFor(pin.Root, pin.Relay, manifest.Share{Session: pin.Session, OwnerPubKey: pin.Owner})()
		if err != nil {
			return fmt.Errorf("relay sign-in: %w", err)
		}
		if nostr.GetPublicKey(rc.Secret).Hex() != pin.Body {
			return errors.New("the enrolled body key is not the pinned body")
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

// sharePin completes an owner pin at install time, run by the owner in
// the terminal: the manifest's relay share owned by owner with a DM
// channel (the --session one when given), its relay URL, DM channel and
// target, and the enrolled body pubkey of that session.
func sharePin(owner, root, session, manifestPath string) (claude.HookPin, error) {
	abs, err := filepath.Abs(root)
	if err != nil || root == "" {
		return claude.HookPin{}, errors.New("an owner pin needs the AMQ root: pass --root or set AM_ROOT")
	}
	if manifestPath == "" {
		manifestPath = manifest.DefaultPath(filepath.Join(abs, stateDirName))
	}
	mf, err := manifest.Load(manifestPath)
	if err != nil {
		return claude.HookPin{}, fmt.Errorf("manifest %s: %w", manifestPath, err)
	}
	var found []manifest.Share
	if mf.Relay != nil {
		for _, sh := range mf.Relay.Shares {
			if sh.OwnerPubKey == owner && sh.DMChannelID != "" && (session == "" || sh.Session == session) {
				found = append(found, sh)
			}
		}
	}
	if len(found) != 1 {
		return claude.HookPin{}, fmt.Errorf("manifest %s has %d relay share(s) with a DM channel for owner %s; pass --session to pick one", manifestPath, len(found), owner)
	}
	sh := found[0]
	creds, err := sharestate.Load(abs, sh.Session)
	if err != nil {
		return claude.HookPin{}, fmt.Errorf("share %s: %w", sh.Session, err)
	}
	if creds.Owner != owner {
		return claude.HookPin{}, fmt.Errorf("share %s is enrolled to another owner", sh.Session)
	}
	return claude.HookPin{Owner: owner, Root: abs, Session: sh.Session, Relay: mf.Relay.URL,
		Body: creds.Body.PublicKeyHex(), Channel: sh.DMChannelID, Target: sh.Target}, nil
}

// installApprovalHook writes the PermissionRequest hook, pinning the owner
// given by --owner (hex or npub) or, without it, the one owner of the
// manifest's relay shares. With no owner found the hook is reject-only.
func installApprovalHook(home string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-approval-hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ownerFlag := fs.String("owner", "", "owner public key (64 hex or npub) whose signed ✅ in Buzz can allow a tool call (default: the relay shares' owner in the manifest)")
	root := fs.String("root", os.Getenv("AM_ROOT"), "AMQ root whose manifest names the share to pin and whose enrolled body key the hook signs in with (default AM_ROOT)")
	session := fs.String("session", "", "the share session to pin (default: the one relay share of the owner with a DM channel)")
	manifestPath := fs.String("manifest", "", "path to the adapter manifest (default: <root>/"+stateDirName+"/manifest.json)")
	if fs.Parse(args) != nil {
		return 2
	}
	owner, err := approvalOwner(*ownerFlag, *root, *manifestPath)
	if err != nil {
		say(stderr, "install-approval-hook: %v\n", err)
		return 1
	}
	var pin claude.HookPin
	if owner != "" {
		if pin, err = sharePin(owner, *root, *session, *manifestPath); err != nil {
			say(stderr, "install-approval-hook: %v\n", err)
			return 1
		}
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
	say(stdout, "approval hook installed: the owner %s can allow a Claude tool call of share %s with ✅ in Buzz, or block it with ❌\n", owner, pin.Session)
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
